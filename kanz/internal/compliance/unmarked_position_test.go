package compliance

import (
	"context"
	"strconv"
	"strings"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// A POSITION THE PLATFORM CANNOT MARK IS NOT A POSITION WORTH ZERO (#760).
//
// heldPositions used to drop any position whose MarketValue was absent, so an
// unmarked holding was removed from the book before any rule saw it. Three
// distinct wrongs came out of that one filter:
//
//  1. The holding escaped every limit written about it — a cap on the instrument
//     itself passed, because the instrument was not in the book.
//  2. Every OTHER bucket's weight was computed against a denominator that had
//     lost it, so the audit trail recorded an observed concentration that was
//     never measured.
//  3. Gross leverage was understated, which is the fail-OPEN direction: a levered
//     book looks safer for holding something nobody could price.
//
// This is #640's shape one hop over. That was an unresolvable DIMENSION and is
// guarded by unresolvedDimension; this is an unreadable VALUE. The exemplar for
// the answer is in this package already — LeverageRule refuses with "leverage
// cannot be verified: NAV unavailable", and venuemargin.go states the principle:
// absent is UNKNOWN and never zero, because a zero is a claim about the account.

// unmarked is a position the platform holds and could not price. Quantity is
// non-zero: this is a real holding, not a flat row retained for history.
func unmarked(inst string) Position {
	return Position{InstrumentID: inst, Quantity: dec(5000, 0), MarketValue: nil}
}

// markedZero is a position priced AT zero — a measured claim, not an absence.
// It must not trip the refusal: conflating the two in the other direction would
// make a worthless holding unevaluable.
func markedZero(inst string) Position {
	return Position{InstrumentID: inst, Quantity: dec(5000, 0), MarketValue: money(0, 0, "USD")}
}

// amountlessMoney carries a currency and NO amount. absRatFromMoney reads it as
// zero exactly like a nil Money, so it has to refuse for the same reason.
func amountless(inst string) Position {
	return Position{
		InstrumentID: inst,
		Quantity:     dec(5000, 0),
		MarketValue:  &commonpb.Money{CurrencyCode: "USD"},
	}
}

func bookWith(extra ...Position) *Book {
	b := &Book{
		PortfolioID: "p1", BaseCurrency: "USD", NAV: money(200000, 0, "USD"),
		NAVBasis:  NAVBasisEquity,
		Positions: []Position{pos("AAPL", 100000, 0, "USD")},
	}
	b.Positions = append(b.Positions, extra...)
	return b
}

func instrumentCap(bucket string, coeff int64, exp int32) *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: "inst-cap", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
		Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
			Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
			Bucket:    bucket,
			MaxWeight: dec(coeff, exp),
		}},
	}
}

func mustRefuse(t *testing.T, res *compliancepb.ComplianceResult, what string) *compliancepb.Violation {
	t.Helper()
	if res.GetStatus() == compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Fatalf("%s PASSED over a book carrying a position nobody could price — the rule could not "+
			"be evaluated and must not read as satisfied: %v", what, res)
	}
	if len(res.GetViolations()) == 0 {
		t.Fatalf("%s: no violation recorded", what)
	}
	return res.GetViolations()[0]
}

// (1) THE UNMARKED HOLDING ESCAPED A CAP WRITTEN ABOUT IT.
func TestConcentration_UnmarkedPositionDoesNotEscapeItsOwnCap(t *testing.T) {
	res := evalStatus(t, bookWith(unmarked("DARK")), nil, instrumentCap("DARK", 10, -2))
	v := mustRefuse(t, res, "a 10% cap on DARK")

	// "Cannot be verified" and "exceeds limit" must not be the same string: an
	// operator reading the audit trail has to tell a breached limit from an
	// unevaluable one. Same distinction classifier_absent_test.go pins.
	if !strings.Contains(v.GetMessage(), "cannot be verified") {
		t.Errorf("message=%q must say the check could not be made, not that a limit was exceeded",
			v.GetMessage())
	}
	if got := v.GetEvidence()["unmarked_holdings"]; got != "1" {
		t.Errorf("evidence[unmarked_holdings]=%q want \"1\" — the count carries the magnitude", got)
	}
	if got := v.GetEvidence()["unmarked_sample"]; !strings.Contains(got, "DARK") {
		t.Errorf("evidence[unmarked_sample]=%q must name DARK — a refusal an operator cannot "+
			"trace to an instrument is a dead end", got)
	}
}

// (2) THE SIBLING'S WEIGHT WAS FABRICATED. Before the fix this returned BREACH
// with observed=1.000000: AAPL was 100% of a book that had silently lost its
// other holding. A breach on invented evidence is not better than a missed one.
func TestConcentration_ASiblingIsNotMeasuredAgainstALostDenominator(t *testing.T) {
	res := evalStatus(t, bookWith(unmarked("DARK")), nil, instrumentCap("AAPL", 60, -2))
	v := mustRefuse(t, res, "a 60% cap on AAPL")

	if got := v.GetEvidence()["observed"]; got != "" {
		t.Errorf("evidence[observed]=%q — the engine reported a measured weight over a book it "+
			"could not total; there is no observation to publish here", got)
	}
	if !strings.Contains(v.GetMessage(), "cannot be verified") {
		t.Errorf("message=%q want a cannot-be-verified refusal", v.GetMessage())
	}
}

// (3) THE FAIL-OPEN DIRECTION, and the reason this is not merely untidy. Gross
// leverage is Σ|market value| / NAV. A dropped holding SHRINKS the numerator, so
// a book carrying something nobody could price reads as LESS levered than it is.
func TestLeverage_AnUnmarkedHoldingDoesNotUnderstateGross(t *testing.T) {
	rule := &compliancepb.Rule{
		RuleId: "lev", Type: compliancepb.RuleType_RULE_TYPE_GROSS_LEVERAGE,
		Params: &compliancepb.Rule_LeverageCap{LeverageCap: &compliancepb.LeverageCap{
			MaxGrossLeverage: dec(100, -2), // 1.0x
		}},
	}
	// AAPL alone is 100000/200000 = 0.5x and passes. The unmarked holding could
	// be any size at all, including one that takes the book through 1.0x.
	res := evalStatus(t, bookWith(unmarked("DARK")), nil, rule)
	v := mustRefuse(t, res, "a 1.0x gross leverage cap")
	if !strings.Contains(v.GetMessage(), "cannot be verified") {
		t.Errorf("message=%q want a cannot-be-verified refusal", v.GetMessage())
	}
}

// A DENY LIST CANNOT BE ENFORCED OVER A BOOK WITH AN UNPRICED HOLDING. "Hold
// nothing in TOBACCO" is not satisfied by a holding the engine could not see —
// the same argument unresolvedDimension makes for an unresolvable dimension.
func TestRestriction_ADenyListIsNotSatisfiedByAnInvisibleHolding(t *testing.T) {
	rule := &compliancepb.Rule{
		RuleId: "deny", Type: compliancepb.RuleType_RULE_TYPE_RESTRICTION,
		Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
			Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
			Values:    []string{"DARK"},
			Mode:      compliancepb.RestrictionMode_RESTRICTION_MODE_DENY,
		}},
	}
	mustRefuse(t, evalStatus(t, bookWith(unmarked("DARK")), nil, rule), "a DENY list naming DARK")
}

// A CURRENCY RESTRICTION CANNOT BE ENFORCED EITHER: the currency is read off the
// market value, so an unmarked holding has no currency to check.
func TestCurrency_AnUnmarkedHoldingHasNoCurrencyToCheck(t *testing.T) {
	rule := &compliancepb.Rule{
		RuleId: "ccy", Type: compliancepb.RuleType_RULE_TYPE_CURRENCY,
		Params: &compliancepb.Rule_CurrencyRestriction{CurrencyRestriction: &compliancepb.CurrencyRestriction{
			AllowedCurrencies: []string{"USD"},
		}},
	}
	mustRefuse(t, evalStatus(t, bookWith(unmarked("DARK")), nil, rule), "a USD-only currency restriction")
}

// A MONEY WITH NO AMOUNT IS THE SAME ABSENCE WEARING A CURRENCY CODE.
// absRatFromMoney reads it as zero exactly like nil, so it must refuse alike —
// otherwise the fix is bypassed by a producer that sets the currency and not
// the number.
func TestConcentration_AMoneyWithNoAmountRefusesLikeAnAbsentOne(t *testing.T) {
	res := evalStatus(t, bookWith(amountless("DARK")), nil, instrumentCap("DARK", 10, -2))
	mustRefuse(t, res, "a 10% cap on an amountless DARK")
}

// NON-VACUITY, and the boundary that keeps the fix honest. A position MARKED at
// zero is a measured claim, not an absence, and must still evaluate. Refusing it
// too would make every worthless holding unevaluable and the refusal meaningless.
func TestConcentration_APositionMarkedAtZeroStillEvaluates(t *testing.T) {
	res := evalStatus(t, bookWith(markedZero("CHEAP")), nil, instrumentCap("AAPL", 60, -2))
	if res.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("AAPL is 100000 of 100000 gross beside a holding priced AT zero — that book is "+
			"fully measured, so the 60%% cap must breach on evidence, never refuse: %v", res)
	}
	v := res.GetViolations()[0]
	// The DISTINCTION this whole change is about: a measured breach says a limit
	// was exceeded and shows the weight; an unevaluable one says the check could
	// not be made. A zero MARK is a measurement, so this must be the former.
	if strings.Contains(v.GetMessage(), "cannot be verified") {
		t.Errorf("message=%q — a holding priced AT zero is a measured claim, and refusing it would "+
			"make every worthless position unevaluable", v.GetMessage())
	}
	if got := v.GetEvidence()["observed"]; got != "1.000000" {
		t.Errorf("evidence[observed]=%q want 1.000000 — this weight was genuinely measured", got)
	}
}

// NON-VACUITY: an ordinary marked book is unaffected in both directions.
func TestConcentration_AFullyMarkedBookIsUnaffected(t *testing.T) {
	if got := evalStatus(t, twoEqualBook(), nil, instrumentCap("AAPL", 60, -2)).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Errorf("50%% under a 60%% cap on a fully marked book: want PASS, got %v", got)
	}
	if got := evalStatus(t, twoEqualBook(), nil, instrumentCap("AAPL", 40, -2)).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Errorf("50%% over a 40%% cap on a fully marked book: want BREACH, got %v", got)
	}
}

// AN EMPTY BOOK IS NOT AN UNMARKED ONE. No holdings means nothing to price, and
// the existing empty-book branches must keep working.
func TestConcentration_AnEmptyBookDoesNotRefuse(t *testing.T) {
	empty := &Book{PortfolioID: "p1", BaseCurrency: "USD", NAV: money(200000, 0, "USD"), NAVBasis: NAVBasisEquity}
	if got := evalStatus(t, empty, nil, instrumentCap("AAPL", 60, -2)).GetStatus(); got != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_PASS {
		t.Errorf("an empty book has nothing concentrated: want PASS, got %v", got)
	}
}

// THE ORDERING CASE, and the one that makes the placement of the check load
// bearing rather than incidental. A book whose EVERY holding is unmarked totals
// zero gross, which is byte-identical to an empty book — so a refusal placed
// after the "empty book: nothing is concentrated" branch would never run, and
// the worst book on the estate would pass every concentration limit.
func TestConcentration_ABookOfOnlyUnmarkedPositionsIsNotAnEmptyBook(t *testing.T) {
	book := &Book{
		PortfolioID: "p1", BaseCurrency: "USD", NAV: money(200000, 0, "USD"),
		NAVBasis:  NAVBasisEquity,
		Positions: []Position{unmarked("DARK"), unmarked("MURK")},
	}
	v := mustRefuse(t, evalStatus(t, book, nil, instrumentCap("DARK", 10, -2)),
		"a 10% cap over a book nothing in which could be priced")
	if got := v.GetEvidence()[EvidenceUnmarkedHoldings]; got != "2" {
		t.Errorf("evidence[%s]=%q want \"2\"", EvidenceUnmarkedHoldings, got)
	}
}

// The evidence keys are asserted through the exported constants as well as the
// literals above, so a rule renaming a key cannot pass by agreeing with a test
// that renamed it too.
func TestUnmarkedEvidenceKeysAreTheDeclaredOnes(t *testing.T) {
	if EvidenceUnmarkedHoldings != "unmarked_holdings" || EvidenceUnmarkedSample != "unmarked_sample" {
		t.Fatalf("evidence keys drifted: %q / %q", EvidenceUnmarkedHoldings, EvidenceUnmarkedSample)
	}
}

// THE COUNT CARRIES THE MAGNITUDE; THE SAMPLE IS BOUNDED. On a book whose marks
// never loaded, every holding is unmarked — putting them all in the violation
// puts them in the audit stream too. So the sample is capped and the COUNT is
// the total, and the two must not be the same number.
//
// Without this the distinction is untestable: with one or two unmarked holdings
// len(sample) and len(total) agree, and a rule reporting the sample size passes
// every other test in this file.
func TestUnmarked_TheCountIsTheTotalAndTheSampleIsBounded(t *testing.T) {
	const n = maxUnresolvedSample + 11 // comfortably past the bound
	book := &Book{
		PortfolioID: "p1", BaseCurrency: "USD", NAV: money(200000, 0, "USD"),
		NAVBasis:  NAVBasisEquity,
		Positions: []Position{pos("AAPL", 100000, 0, "USD")},
	}
	for i := 0; i < n; i++ {
		book.Positions = append(book.Positions, unmarked("DARK-"+strconv.Itoa(i)))
	}

	v := mustRefuse(t, evalStatus(t, book, nil, instrumentCap("AAPL", 60, -2)),
		"a 60% cap over a book with many unmarked holdings")

	if got := v.GetEvidence()[EvidenceUnmarkedHoldings]; got != strconv.Itoa(n) {
		t.Errorf("evidence[%s]=%q want %q — the count must be the TOTAL, not the sample size, or "+
			"an operator cannot tell 33 unpriced holdings from a whole book of them",
			EvidenceUnmarkedHoldings, got, strconv.Itoa(n))
	}
	sample := v.GetEvidence()[EvidenceUnmarkedSample]
	if got := len(strings.Split(sample, ",")); got != maxUnresolvedSample {
		t.Errorf("sample carries %d instruments, want it capped at %d — an unbounded list puts the "+
			"whole book on every violation and into the audit stream", got, maxUnresolvedSample)
	}
}

// THE INSTITUTIONAL CONSEQUENCE: THE ORDER IS ACTUALLY STOPPED (#760).
//
// Everything above asserts on a Violation object. This asserts on the thing that
// matters — PreTradeGate.Evaluate returning Allowed=false — because a rule that
// produces a violation the gate then admits has changed nothing. The gate admits
// on anything short of BREACH, so the refusal must carry the rule's own severity
// through deny-by-default, exactly as the classifier refusal does.
func TestPreTradeGate_RefusesAnOrderAgainstABookItCannotPrice(t *testing.T) {
	book := &Book{
		PortfolioID: "p1", BaseCurrency: "USD", NAV: money(200000, 0, "USD"),
		NAVBasis: NAVBasisEquity,
		Positions: []Position{
			{InstrumentID: "AAPL", Quantity: dec(100, 0), MarketValue: money(100000, 0, "USD")},
			{InstrumentID: "MSFT", Quantity: dec(100, 0), MarketValue: money(100000, 0, "USD")},
			unmarked("DARK"),
		},
	}
	reg := NewMandateRegistry()
	mustPut(t, reg, concentrationMandate(60))
	g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": book}, reg, nil, nil, nil)

	// THE MARKED HALF MUST STAY COMPLIANT ON ITS OWN, or this test passes for the
	// wrong reason. AAPL 110k of the 210k the engine can see is ~52%, comfortably
	// inside the 60% cap — so without the refusal the gate ADMITS, and the only
	// thing that can reject here is the unmarked holding. An earlier version of
	// this fixture omitted MSFT, which made AAPL 100% of the visible gross and
	// breached on concentration whether or not the fix was present.
	got, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(10, 0), Price: dec(1000, 0), Currency: "USD",
		OrderID: "o-unmarked", AsOf: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Allowed {
		t.Fatalf("the gate ADMITTED an order against a book carrying a holding nobody could "+
			"price — the concentration cap was evaluated over a fraction of the book and the "+
			"answer means nothing: %v", got.Result)
	}
	if got.Result.GetStatus() != compliancepb.ComplianceStatus_COMPLIANCE_STATUS_BREACH {
		t.Fatalf("status=%v want BREACH — the rule declares no on_violation, so deny-by-default "+
			"applies, same as the classifier refusal", got.Result.GetStatus())
	}
}

// NON-VACUITY for the gate: the same order over a fully marked book is admitted.
// Without this, the refusal above is satisfied by a gate that refuses everything.
func TestPreTradeGate_StillAdmitsAgainstAFullyMarkedBook(t *testing.T) {
	reg := NewMandateRegistry()
	mustPut(t, reg, concentrationMandate(60))
	g := NewPreTradeGate(NewEngine(nil), MapBookSource{"p1": currentBook()}, reg, nil, nil, nil)

	got, err := g.Evaluate(context.Background(), OrderDelta{
		TenantID: "t1", PortfolioID: "p1", InstrumentID: "AAPL",
		SignedQuantity: dec(10, 0), Price: dec(1000, 0), Currency: "USD",
		OrderID: "o-marked", AsOf: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allowed {
		t.Fatalf("a compliant order over a fully marked book must still be admitted: %v", got.Result)
	}
}
