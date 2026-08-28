package compliance

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/oms/internal/cashview"
)

// THE ONLY PLACE ON THE PLATFORM WHERE EQUITY CAN BE COMPUTED (#780).
//
// The position book folds fills, so it values holdings at what they COST and
// reports the sum of those as NAV. Gross exposure is the sum of the absolute
// values of the same holdings, so that denominator made every long-only book
// score exactly 1.0 and a max_gross_leverage cap could not bind.
//
// BookSource is the join: holdings from the position book, prices from the mark
// fold, cash from the book of record. These tests pin both halves of the
// contract — that it produces equity when it can, and that it changes NOTHING
// when it cannot.

// marks is a stub MarkSource. A missing entry returns nil, which is what
// mark.Source does for an instrument that was never seen AND for one whose
// price has expired — the two states it refuses to distinguish on a decision
// path.
type marks map[string]*big.Rat

func (m marks) Mark(instrument string) *big.Rat { return m[instrument] }

func at(coeff int64) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: coeff} }

func usdMoney(coeff int64) *commonpb.Money {
	return &commonpb.Money{Amount: at(coeff), CurrencyCode: "USD"}
}

// bookWith builds a snapshot the way position.Book does: every holding valued at
// AVERAGE COST, and TotalMarketValue as the sum of those costs.
func bookWith(positions ...*domainpb.PositionState) *domainpb.PortfolioSnapshot {
	total := new(big.Rat)
	for _, p := range positions {
		total.Add(total, dec.FromProto(p.GetMarketValue().GetAmount()))
	}
	amt, _ := dec.ToProtoScaled(total)
	return &domainpb.PortfolioSnapshot{
		Portfolio: &domainpb.PortfolioState{
			PortfolioId:      "PF1",
			BaseCurrency:     "USD",
			TotalMarketValue: &commonpb.Money{Amount: amt, CurrencyCode: "USD"},
		},
		Positions: positions,
	}
}

func held(instrument string, qty, cost int64) *domainpb.PositionState {
	return &domainpb.PositionState{
		PortfolioId: "PF1", InstrumentId: instrument,
		Quantity:    at(qty),
		MarketValue: usdMoney(cost),
	}
}

// cashAt folds one accounting announcement, so the test drives the same view the
// OMS runs rather than a hand-built stub.
func cashAt(t *testing.T, total int64) *cashview.View {
	t.Helper()
	v := cashview.New()
	announce(t, v, "PF1", total, time.Now().UTC())
	return v
}

func bookFrom(t *testing.T, snap *domainpb.PortfolioSnapshot, cash CashSource, m MarkSource) *comp.Book {
	t.Helper()
	b, err := NewBookSource(stubStore{snap}, cash, nil, m).Book(context.Background(), "PF1")
	if err != nil {
		t.Fatalf("Book: %v", err)
	}
	return b
}

// TestEquityIsMarkedPositionsPlusCash is the whole point: the book comes back
// marked to market with a real equity figure, and BOTH numbers move off the
// cost basis together.
func TestEquityIsMarkedPositionsPlusCash(t *testing.T) {
	// 100 AAPL bought for $10,000 total, now marked at $150 ⇒ $15,000.
	// 50 MSFT bought for $10,000 total, now marked at $300 ⇒ $15,000.
	// Cash $5,000. Equity = 15,000 + 15,000 + 5,000 = $35,000.
	snap := bookWith(held("AAPL", 100, 10_000), held("MSFT", 50, 10_000))
	b := bookFrom(t, snap, cashAt(t, 5_000), marks{
		"AAPL": big.NewRat(150, 1),
		"MSFT": big.NewRat(300, 1),
	})

	if b.NAVBasis != comp.NAVBasisEquity {
		t.Fatalf("NAVBasis = %v, want equity — every holding was markable and cash was known, so "+
			"there is nothing left to stop this book being valued (detail: %q)", b.NAVBasis, b.NAVBasisDetail)
	}
	if got := dec.Str(dec.FromProto(b.NAV.GetAmount())); got != "35000" {
		t.Errorf("equity = %s, want 35000 (15000 + 15000 marked, + 5000 cash)", got)
	}
	if b.NAV.GetCurrencyCode() != "USD" {
		t.Errorf("equity currency = %q, want USD", b.NAV.GetCurrencyCode())
	}
	if b.NAVBasisDetail != "" {
		t.Errorf("NAVBasisDetail = %q, want empty on success", b.NAVBasisDetail)
	}

	// THE POSITIONS MOVED TOO, and that is not cosmetic. The numerator
	// LeverageRule divides INTO this equity is the sum of these values; leaving
	// them at cost while the denominator is at market would produce a ratio that
	// is neither number.
	for _, p := range b.Positions {
		got := dec.Str(dec.FromProto(p.MarketValue.GetAmount()))
		if got != "15000" {
			t.Errorf("%s market value = %s, want 15000 — the holding is still at cost", p.InstrumentID, got)
		}
	}
}

// TestAnEmptyBookIsWorthItsCash is the 100%-cash portfolio, and the reason
// "every position is marked" has to be vacuously true rather than a special
// case: a fund that has been funded and has bought nothing has equity, and
// refusing to say so would refuse its first order.
func TestAnEmptyBookIsWorthItsCash(t *testing.T) {
	b := bookFrom(t, bookWith(), cashAt(t, 250_000), marks{})

	if b.NAVBasis != comp.NAVBasisEquity {
		t.Fatalf("NAVBasis = %v, want equity for a funded book with no holdings (detail: %q)",
			b.NAVBasis, b.NAVBasisDetail)
	}
	if got := dec.Str(dec.FromProto(b.NAV.GetAmount())); got != "250000" {
		t.Errorf("equity = %s, want 250000 — a book with no positions is worth its cash", got)
	}
}

// TestEquityIsAllOrNothing is the property that bounds this change's blast
// radius, and each case asserts the SAME three things: the basis did not move,
// the reason is legible, and the book was left exactly as it was found.
func TestEquityIsAllOrNothing(t *testing.T) {
	// One instrument priced, one not. AAPL's cost basis is deliberately different
	// from what its mark would give, so a partial upgrade is detectable.
	twoHoldings := func() *domainpb.PortfolioSnapshot {
		return bookWith(held("AAPL", 100, 10_000), held("SOL-USD", 10, 1_000))
	}

	cases := []struct {
		name       string
		snap       *domainpb.PortfolioSnapshot
		cash       CashSource
		marks      MarkSource
		wantDetail string
	}{{
		name: "one instrument has no live mark",
		snap: twoHoldings(),
		cash: cashAt(t, 5_000),
		// mark.Source returns nil for never-seen and for expired alike.
		marks:      marks{"AAPL": big.NewRat(150, 1)},
		wantDetail: "no live mark for SOL-USD",
	}, {
		name:       "cash was never announced",
		snap:       twoHoldings(),
		cash:       nil,
		marks:      marks{"AAPL": big.NewRat(150, 1), "SOL-USD": big.NewRat(100, 1)},
		wantDetail: "cash is unknown",
	}, {
		name:       "a holding is denominated in another currency",
		snap:       eurHolding(),
		cash:       cashAt(t, 5_000),
		marks:      marks{"SAP": big.NewRat(150, 1)},
		wantDetail: "held in EUR",
	}, {
		name:       "a holding carries no quantity",
		snap:       bookWith(&domainpb.PositionState{PortfolioId: "PF1", InstrumentId: "AAPL", MarketValue: usdMoney(10_000)}),
		cash:       cashAt(t, 5_000),
		marks:      marks{"AAPL": big.NewRat(150, 1)},
		wantDetail: "carries no quantity",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := bookFrom(t, tc.snap, nil, nil) // the untouched book, for comparison
			b := bookFrom(t, tc.snap, tc.cash, tc.marks)

			if b.NAVBasis != comp.NAVBasisGrossPositions {
				t.Fatalf("NAVBasis = %v, want gross_positions — equity could not be established, and "+
					"claiming it anyway is the defect (#780)", b.NAVBasis)
			}
			if !strings.Contains(b.NAVBasisDetail, tc.wantDetail) {
				t.Errorf("NAVBasisDetail = %q, want it to contain %q. The refusal downstream is "+
					"unactionable without it", b.NAVBasisDetail, tc.wantDetail)
			}
			// NOTHING WAS PARTIALLY UPGRADED. A book marked as far as the failure and
			// left at cost after it would give a numerator that is a mixture of two
			// bases — precise, plausible, and meaningless.
			if got, want := moneyStr(b.NAV), moneyStr(before.NAV); got != want {
				t.Errorf("NAV = %s, want the untouched %s", got, want)
			}
			for i, p := range b.Positions {
				if got, want := moneyStr(p.MarketValue), moneyStr(before.Positions[i].MarketValue); got != want {
					t.Errorf("%s market value = %s, want the untouched %s — the book was partially "+
						"re-marked before the failure", p.InstrumentID, got, want)
				}
			}
		})
	}
}

// eurHolding is a book whose position is denominated in something other than the
// portfolio's base currency — the one case where "the mark is in base currency",
// which COMP01Gate already assumes when it values a MARKET order, is
// demonstrably false.
func eurHolding() *domainpb.PortfolioSnapshot {
	snap := bookWith(held("SAP", 100, 10_000))
	snap.Positions[0].MarketValue = &commonpb.Money{Amount: at(10_000), CurrencyCode: "EUR"}
	return snap
}

func moneyStr(m *commonpb.Money) string {
	if m == nil {
		return "<nil>"
	}
	return dec.Str(dec.FromProto(m.GetAmount())) + " " + m.GetCurrencyCode()
}

// TestNoMarkSourceIsAPostureAndNotAFault. A deployment with no price feed is a
// documented posture the gate already announces at startup. Recording a
// per-portfolio "reason" for it would put a fault-shaped string in front of an
// operator for a configuration somebody chose.
func TestNoMarkSourceIsAPostureAndNotAFault(t *testing.T) {
	b := bookFrom(t, bookWith(held("AAPL", 100, 10_000)), cashAt(t, 5_000), nil)

	if b.NAVBasis != comp.NAVBasisGrossPositions {
		t.Fatalf("NAVBasis = %v, want gross_positions with no mark source", b.NAVBasis)
	}
	if b.NAVBasisDetail != "" {
		t.Errorf("NAVBasisDetail = %q, want empty — nothing tried, so there is nothing to explain",
			b.NAVBasisDetail)
	}
}

// TestALeveredBookIsRefusedThroughTheWholeChain is the issue's acceptance
// criterion driven end to end: position book → mark fold → cash view →
// BookSource → the pre-trade gate → a mandate carrying a real leverage cap.
//
// It is the test that fails if any two of those links stop agreeing, which is
// the failure that would otherwise surface only as orders being admitted in
// production for a reason nobody could attribute.
func TestALeveredBookIsRefusedThroughTheWholeChain(t *testing.T) {
	// $150,000 of holdings (1,000 AAPL marked at $150) against $50,000 of cash
	// borrowed against — equity $100,000, gross $150,000, leverage 1.5x.
	//
	// Cash is NEGATIVE because that is what financing a position looks like: the
	// margin loan is the liability equity nets against. Under the old
	// positions-only denominator this book scored 150,000/150,000 = 1.0 and
	// passed.
	snap := bookWith(held("AAPL", 1_000, 100_000))
	src := NewBookSource(stubStore{snap}, cashAt(t, -50_000), nil, marks{"AAPL": big.NewRat(150, 1)})
	gate := comp.NewPreTradeGate(comp.NewEngine(nil), src,
		stubMandate{leverageMandate("PF1", 12, -1)}, nil, nil, nil) // cap 1.2x

	verdict, err := gate.Evaluate(context.Background(), comp.OrderDelta{
		TenantID: "t1", PortfolioID: "PF1", InstrumentID: "AAPL",
		SignedQuantity: at(1), Price: at(150), Currency: "USD", OrderID: "o1",
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if verdict.Allowed {
		t.Fatal("a book at 1.5x gross leverage was ADMITTED under a 1.2x cap. Every link in the " +
			"chain reported success and the control did not fire — which is #780 exactly: the " +
			"denominator was the sum of the same positions the numerator sums")
	}

	// AND THE SAME CHAIN ADMITS A BOOK THAT IS INSIDE ITS CAP, so the assertion
	// above is not satisfied by a gate that refuses everything.
	funded := NewBookSource(stubStore{snap}, cashAt(t, 900_000), nil, marks{"AAPL": big.NewRat(150, 1)})
	gate = comp.NewPreTradeGate(comp.NewEngine(nil), funded,
		stubMandate{leverageMandate("PF1", 12, -1)}, nil, nil, nil)
	got, err := gate.Evaluate(context.Background(), comp.OrderDelta{
		TenantID: "t1", PortfolioID: "PF1", InstrumentID: "AAPL",
		SignedQuantity: at(1), Price: at(150), Currency: "USD", OrderID: "o2",
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !got.Allowed {
		t.Fatal("a book holding $150k against $1.05m of equity — 0.14x — was refused under a 1.2x cap")
	}
}

func leverageMandate(portfolioID string, limit int64, exp int32) *compliancepb.Mandate {
	return &compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: portfolioID,
		Rules: []*compliancepb.Rule{{
			RuleId: "lev-1",
			Type:   compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE,
			Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{
				MaxGrossLeverage: &commonpb.Decimal{Coefficient: limit, Exponent: exp},
			}},
		}},
	}
}
