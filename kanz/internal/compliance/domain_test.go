package compliance

import (
	"context"
	"testing"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
)

// concentrationRule builds a bare RULE_TYPE_CONCENTRATION rule capped at
// maxPct percent of gross, on the instrument dimension. There is no shared
// helper of this exact shape in the package: gate_test.go's
// concentrationMandate wraps mandate(...) itself, and arithmetic_sweep_test.go
// builds the *compliancepb.Rule literal inline per-arm. This mirrors both.
func concentrationRule(maxPct int64) *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: "c1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
			MaxWeight: dec(maxPct, -2),
		}},
	}
}

// domainGate builds a governed gate over a book holding $100,000 of AAPL
// alongside an untouched $100,000 of MSFT, so an order reaches the rules
// rather than being short-circuited as ungoverned. AAPL sits at 50% of gross
// against a 60% concentration cap — deliberately a SECOND position: with only
// AAPL held, its weight is 100% of gross by construction and the fixture
// would breach concentration on its own, before any order, making
// TestEvaluate_NormalOrderStillAdmitted fail regardless of the domain bound
// (mirrors gate_test.go's currentBook/concentrationMandate pairing, which has
// the same two-position shape for the same reason).
func domainGate(t *testing.T) *PreTradeGate {
	t.Helper()
	reg := NewMandateRegistry()
	reg.Put(mandate(concentrationRule(60)))
	books := MapBookSource{"p1": &Book{
		PortfolioID:  "p1",
		BaseCurrency: "USD",
		NAV:          &commonpb.Money{Amount: dec(200000, 0), CurrencyCode: "USD"},
		Positions: []Position{
			{
				InstrumentID: "AAPL",
				Quantity:     dec(1000, 0),
				MarketValue:  &commonpb.Money{Amount: dec(100000, 0), CurrencyCode: "USD"},
			},
			{
				InstrumentID: "MSFT",
				Quantity:     dec(1000, 0),
				MarketValue:  &commonpb.Money{Amount: dec(100000, 0), CurrencyCode: "USD"},
			},
		},
	}}
	return NewPreTradeGate(nil, books, reg, nil, nil, nil)
}

func domainDelta(qty *commonpb.Decimal) OrderDelta {
	return OrderDelta{
		PortfolioID:    "p1",
		InstrumentID:   "AAPL",
		SignedQuantity: qty,
		// $100.00 at exponent -2, not a suspiciously round exponent-0 value: a
		// cents-denominated price is what a real order actually carries, and it
		// gives the Step-5 "lower the bound to 1" mutation something genuine to
		// refuse (exponent 0 is in-domain at every positive bound, so an
		// all-exponent-0 fixture can never prove the bound is not over-broad).
		Price:    dec(10000, -2),
		Currency: "USD",
		OrderID:  "o1",
		AsOf:     t0,
	}
}

// THE LIVE HANG. An order in an UNHELD instrument carrying {0, 2e9} made
// ratFromDecimal materialise 10^2000000000. This must refuse, promptly.
//
// The instrument must be unheld for this to reproduce the live defect:
// against a HELD instrument, addDecimal's zero-coefficient guard (mul_test.go,
// TestAddDecimal_ZeroCoefficientDoesNotAnnihilateTheOtherOperand) already
// aligns a zero-coefficient delta to the EXISTING position's sane exponent
// before anything downstream sees it, independent of this bound. Against an
// unheld instrument there is no existing exponent to align to — addDecimal
// passes the delta's own exponent straight through into the projected
// position, and only THIS check stops it from reaching ratFromDecimal.
//
// Guarded STRUCTURALLY rather than with a timeout: a timeout bounds the test,
// not the process, and Go cannot cancel the goroutine that would still be
// grinding a multi-billion-digit bignum. If the bound regresses, the assertion
// below never returns and CI fails on its own panic timeout — which is loud,
// but the real protection is that decimalInDomain is O(1) by construction.
func TestEvaluate_OutOfDomainExponentIsRefused(t *testing.T) {
	g := domainGate(t)
	delta := domainDelta(&commonpb.Decimal{Coefficient: 0, Exponent: 2000000000})
	delta.InstrumentID = "GOOG" // unheld in domainGate's book — see comment above
	d, err := g.Evaluate(context.Background(), delta)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if d.Allowed {
		t.Fatal("an order carrying exponent 2e9 was ADMITTED")
	}
	if !d.Unvaluable {
		t.Fatalf("want Unvaluable, got %+v — out-of-domain input must refuse through "+
			"the existing path, not invent a new one", d)
	}
}

func TestEvaluate_DomainBoundaryInBothSigns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		exp     int32
		refused bool
	}{
		{"at the positive bound", 64, false},
		{"past the positive bound", 65, true},
		{"at the negative bound", -64, false},
		{"past the negative bound", -65, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := domainGate(t)
			d, err := g.Evaluate(context.Background(), domainDelta(&commonpb.Decimal{
				Coefficient: 1, Exponent: tc.exp,
			}))
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if d.Unvaluable != tc.refused {
				t.Fatalf("exponent %d: Unvaluable = %v, want %v", tc.exp, d.Unvaluable, tc.refused)
			}
		})
	}
}

// The book is validated too. It is built from venue fills, so it is externally
// influenced; validating only the order would leave the same hang reachable
// through a corrupted position.
//
// The absurd exponent goes on MarketValue, not Quantity: ConcentrationRule
// (rules.go, via totalGross/heldPositions/absRatFromMoney) reads a position's
// MarketValue through ratFromDecimal directly, with no addDecimal/mulDecimal
// step in between to insulate it — the same unguarded path LeverageRule takes
// through Book.NAV. Quantity is not on that path for this rule, so an absurd
// Quantity alone would not reproduce the hang this test guards against.
func TestEvaluate_OutOfDomainBookPositionIsRefused(t *testing.T) {
	reg := NewMandateRegistry()
	reg.Put(mandate(concentrationRule(60)))
	books := MapBookSource{"p1": &Book{
		PortfolioID:  "p1",
		BaseCurrency: "USD",
		NAV:          &commonpb.Money{Amount: dec(100000, 0), CurrencyCode: "USD"},
		Positions: []Position{{
			InstrumentID: "MSFT",
			Quantity:     dec(1, 0),
			MarketValue:  &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1, Exponent: 2000000000}, CurrencyCode: "USD"},
		}},
	}}
	g := NewPreTradeGate(nil, books, reg, nil, nil, nil)

	d, err := g.Evaluate(context.Background(), domainDelta(dec(10, 0)))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !d.Unvaluable {
		t.Fatalf("a book position carrying exponent 2e9 was not refused: %+v", d)
	}
}

// The check runs BEFORE the ungoverned branch, so malformed input is refused
// even where no mandate governs the portfolio. That is a deliberate behaviour
// change on that path: this is input validation, not a compliance question.
func TestEvaluate_OutOfDomainRefusedEvenWhenUngoverned(t *testing.T) {
	g := NewPreTradeGate(nil, MapBookSource{}, NewMandateRegistry(), nil, nil, nil)
	d, err := g.Evaluate(context.Background(), domainDelta(&commonpb.Decimal{
		Coefficient: 1, Exponent: 2000000000,
	}))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if d.Allowed {
		t.Fatal("an out-of-domain order was admitted as ungoverned — the domain check " +
			"must run before the mandate lookup")
	}
}

// NON-VACUITY, and the most important test here. Every guard in this change
// fails closed, so a bound that refused EVERYTHING would satisfy all four tests
// above. A normal order must still reach the rules and be admitted.
func TestEvaluate_NormalOrderStillAdmitted(t *testing.T) {
	g := domainGate(t)
	d, err := g.Evaluate(context.Background(), domainDelta(dec(10, 0)))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if d.Unvaluable {
		t.Fatal("a normal 10-share order was refused as unvaluable — the bound is " +
			"rejecting real orders")
	}
	if !d.Allowed {
		t.Fatalf("a normal 10-share order was not admitted: %+v", d)
	}
}

// A nil Decimal is in-domain: absent is not out-of-range, and the existing
// unpriced check already owns that case.
func TestDecimalInDomain_NilIsInDomain(t *testing.T) {
	if !decimalInDomain(nil) {
		t.Fatal("nil must be in-domain — absence is the unpriced check's business")
	}
}

// Zero is in-domain at a sane exponent, but its EXPONENT is still checked.
// {0, 2e9} is precisely the verified hang, so "it's zero, skip it" is the
// shortcut that would reintroduce it.
func TestDecimalInDomain_ZeroIsCheckedByExponent(t *testing.T) {
	if !decimalInDomain(&commonpb.Decimal{Coefficient: 0, Exponent: 0}) {
		t.Fatal("plain zero must be in-domain")
	}
	if decimalInDomain(&commonpb.Decimal{Coefficient: 0, Exponent: 2000000000}) {
		t.Fatal("zero with an absurd exponent must be OUT of domain — a zero " +
			"coefficient does not make 10^2e9 cheap to compute")
	}
}
