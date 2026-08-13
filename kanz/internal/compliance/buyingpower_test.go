package compliance

import (
	"context"
	"math/big"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// CAN THE FUND AFFORD THIS ORDER? (#415 step 4)
//
// Every other rule in this package bounds the SHAPE of the book — how
// concentrated, how levered, which instruments and currencies. None asked
// whether the portfolio has the money. Margin sufficiency was discovered from
// the exchange, AFTER dispatch, which is the wrong side of the trade to find out
// on.
//
// The half that makes this real rather than decorative is project(): it now
// DEBITS the hypothetical trade. Without that the rule compares PRE-trade cash
// against the floor, so every order the portfolio could already afford passes —
// including the one that spends the last of it. The rule would never fire and
// its silence would read as compliance.

func buyingPowerRule(floor *commonpb.Decimal) *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: "bp-1",
		Type:   compliancepb.RuleType_RULE_TYPE_BUYING_POWER,
		Params: &compliancepb.Rule_BuyingPower{BuyingPower: &compliancepb.BuyingPowerLimit{MinCashAfter: floor}},
	}
}

func bookWithCash(cash *commonpb.Money) *Book {
	return &Book{PortfolioID: "PF1", BaseCurrency: "USD", NAV: money(1_000_000, 0, "USD"), Cash: cash}
}

func TestBuyingPowerRule_AdmitsWhenCashRemainsAboveTheFloor(t *testing.T) {
	c := &Candidate{Book: bookWithCash(money(500, 0, "USD"))}
	if v := BuyingPowerRule(c, buyingPowerRule(nil)); v != nil {
		t.Fatalf("500 cash against a zero floor was refused: %v", v)
	}
}

func TestBuyingPowerRule_RefusesWhenCashFallsBelowTheFloor(t *testing.T) {
	// Post-trade cash is NEGATIVE — the order spent money the portfolio does not
	// have. With no floor set, zero is the floor.
	c := &Candidate{Book: bookWithCash(money(-250, 0, "USD"))}
	v := BuyingPowerRule(c, buyingPowerRule(nil))
	if v == nil {
		t.Fatal("an order that drives cash negative was admitted — the fund cannot pay for it")
	}
	// Compared numerically: ratString pads to a fixed scale, and pinning its
	// spelling would make this test about the formatter rather than the balance.
	if got := v.GetEvidence()["cash_after"]; ratFromString(t, got).Cmp(big.NewRat(-250, 1)) != 0 {
		t.Errorf("evidence cash_after = %q, want -250", got)
	}
}

// A RESERVE IS A FLOOR ABOVE ZERO. The usual shape for a margin buffer: cash the
// fund declines to commit.
func TestBuyingPowerRule_HonoursAPositiveFloor(t *testing.T) {
	c := &Candidate{Book: bookWithCash(money(100, 0, "USD"))}
	// 100 left, but the mandate insists on keeping 250.
	if v := BuyingPowerRule(c, buyingPowerRule(&commonpb.Decimal{Coefficient: 250})); v == nil {
		t.Fatal("cash fell below a positive reserve and the order was admitted")
	}
	// And the same book passes when the reserve is 100.
	if v := BuyingPowerRule(c, buyingPowerRule(&commonpb.Decimal{Coefficient: 100})); v != nil {
		t.Fatalf("cash exactly at the floor was refused: %v — the floor is a minimum, not a strict bound", v)
	}
}

// ABSENT CASH FAILS CLOSED. Treating unknown cash as unlimited would admit every
// order while a mandate declares a spending limit — a control that reports
// success, which is the failure mode this platform designs against.
//
// This is the state a deployment fed by the OMS position book is in today: its
// Snapshot sets TotalMarketValue and not CashBalance. The rule must refuse
// LOUDLY there, naming the reason, rather than pass.
func TestBuyingPowerRule_FailsClosedWhenCashIsUnknown(t *testing.T) {
	c := &Candidate{Book: bookWithCash(nil)}
	v := BuyingPowerRule(c, buyingPowerRule(nil))
	if v == nil {
		t.Fatal("a portfolio whose cash the platform cannot establish was admitted — " +
			"unknown affordability is not affordability")
	}
	if v.GetEvidence()["cash"] != "unavailable" {
		t.Errorf("evidence = %v, want cash=unavailable so an operator can tell this from a real breach",
			v.GetEvidence())
	}
}

func TestBuyingPowerRule_RefusesAMismatchedParam(t *testing.T) {
	c := &Candidate{Book: bookWithCash(money(500, 0, "USD"))}
	wrong := &compliancepb.Rule{RuleId: "bp-2", Type: compliancepb.RuleType_RULE_TYPE_BUYING_POWER}
	if v := BuyingPowerRule(c, wrong); v == nil {
		t.Fatal("a BUYING_POWER rule carrying no buying_power params was admitted")
	}
}

// THE DEBIT IS THE WHOLE THING (#415).
//
// project() must move cash by -(signed_quantity x price). This drives the gate's
// own projection rather than calling the rule directly, because a rule that is
// correct against a book nobody debits is a rule that never fires.
func TestProject_DebitsTheTradeFromCash(t *testing.T) {
	book := &Book{
		PortfolioID: "PF1", BaseCurrency: "USD",
		NAV:  money(1_000_000, 0, "USD"),
		Cash: money(1000, 0, "USD"),
	}
	// BUY 3 at 100 ⇒ spends 300 ⇒ 700 left.
	buy := OrderDelta{
		InstrumentID:   "AAPL",
		SignedQuantity: &commonpb.Decimal{Coefficient: 3},
		Price:          &commonpb.Decimal{Coefficient: 100},
		Currency:       "USD",
	}
	proj, ok := project(book, buy)
	if !ok {
		t.Fatal("project refused a representable order")
	}
	if got := ratFromDecimal(proj.Cash.GetAmount()); got.Cmp(big.NewRat(700, 1)) != 0 {
		t.Fatalf("cash after a 3x100 BUY = %s, want 700. Without the debit the rule compares "+
			"PRE-trade cash and never fires.", got.RatString())
	}
	// The original book is untouched — project works on a clone.
	if got := ratFromDecimal(book.Cash.GetAmount()); got.Cmp(big.NewRat(1000, 1)) != 0 {
		t.Fatalf("project mutated the caller's book: cash = %s, want 1000", got.RatString())
	}
}

// A SELL RAISES CASH. Without this, "debit the trade" is satisfied by always
// subtracting, which would refuse the very orders that fund the account.
func TestProject_CreditsCashOnASell(t *testing.T) {
	book := &Book{
		PortfolioID: "PF1", BaseCurrency: "USD",
		NAV:  money(1_000_000, 0, "USD"),
		Cash: money(1000, 0, "USD"),
		Positions: []Position{{
			InstrumentID: "AAPL",
			Quantity:     &commonpb.Decimal{Coefficient: 10},
			MarketValue:  money(1000, 0, "USD"),
		}},
	}
	sell := OrderDelta{
		InstrumentID:   "AAPL",
		SignedQuantity: &commonpb.Decimal{Coefficient: -4},
		Price:          &commonpb.Decimal{Coefficient: 100},
		Currency:       "USD",
	}
	proj, ok := project(book, sell)
	if !ok {
		t.Fatal("project refused a representable sell")
	}
	if got := ratFromDecimal(proj.Cash.GetAmount()); got.Cmp(big.NewRat(1400, 1)) != 0 {
		t.Fatalf("cash after a 4x100 SELL = %s, want 1400", got.RatString())
	}
}

// A BOOK WITH NO CASH STAYS NIL through the projection, so the rule sees
// "unknown" and fails closed rather than seeing a fabricated zero that would
// look like an empty account.
func TestProject_LeavesUnknownCashUnknown(t *testing.T) {
	book := &Book{PortfolioID: "PF1", BaseCurrency: "USD", NAV: money(1_000_000, 0, "USD")}
	proj, ok := project(book, OrderDelta{
		InstrumentID:   "AAPL",
		SignedQuantity: &commonpb.Decimal{Coefficient: 3},
		Price:          &commonpb.Decimal{Coefficient: 100},
		Currency:       "USD",
	})
	if !ok {
		t.Fatal("project refused a representable order")
	}
	if proj.Cash != nil {
		t.Fatalf("unknown cash became %v — a fabricated zero reads as an empty account, which "+
			"is a breach rather than an unknown", proj.Cash)
	}
}

// END TO END THROUGH THE GATE: a mandate declaring a buying-power limit refuses
// an order the portfolio cannot pay for, and admits one it can.
func TestGate_RefusesAnOrderThePortfolioCannotAfford(t *testing.T) {
	book := &Book{
		PortfolioID: "PF1", BaseCurrency: "USD",
		NAV:  money(1_000_000, 0, "USD"),
		Cash: money(250, 0, "USD"),
	}
	mandate := &compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: "PF1",
		Rules: []*compliancepb.Rule{buyingPowerRule(nil)},
	}
	g := NewPreTradeGate(nil, staticBook{book}, staticMandate{mandate}, nil, nil, nil)

	// BUY 3 at 100 = 300, against 250 of cash.
	res, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID: "t1", PortfolioID: "PF1", InstrumentID: "AAPL",
		SignedQuantity: &commonpb.Decimal{Coefficient: 3},
		Price:          &commonpb.Decimal{Coefficient: 100},
		Currency:       "USD", OrderID: "o1",
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.Allowed {
		t.Fatal("an order for 300 against 250 of cash was admitted at the gate")
	}

	// The same portfolio can afford 2 at 100.
	res, err = g.Evaluate(context.Background(), OrderDelta{
		TenantID: "t1", PortfolioID: "PF1", InstrumentID: "AAPL",
		SignedQuantity: &commonpb.Decimal{Coefficient: 2},
		Price:          &commonpb.Decimal{Coefficient: 100},
		Currency:       "USD", OrderID: "o2",
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !res.Allowed {
		t.Fatalf("an affordable order was refused: %v — a gate that refuses everything is an "+
			"outage, not a control", res.Result.GetViolations())
	}
}

type staticBook struct{ b *Book }

func (s staticBook) Book(context.Context, string) (*Book, error) { return s.b, nil }

type staticMandate struct{ m *compliancepb.Mandate }

func (s staticMandate) Mandate(_ context.Context, _, _ string, _ time.Time) (*compliancepb.Mandate, bool, error) {
	return s.m, true, nil
}

// ratFromString parses evidence back to an exact rational, so an assertion is
// about the NUMBER and not about how ratString happens to pad it.
func ratFromString(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("evidence %q is not a decimal", s)
	}
	return r
}
