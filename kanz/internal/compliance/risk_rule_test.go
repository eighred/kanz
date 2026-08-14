package compliance

import (
	"math/big"
	"strings"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// THE RISK HALF OF THE PRE-TRADE GATE (#438).
//
// Every other rule in this package bounds the SHAPE of the book from holdings the
// OMS already has. This one asks what those holdings are worth LOSING — a number
// only the risk engine computes — and until it existed, an order could sit inside
// every mandate rule on this platform and still take the portfolio through its
// VaR limit. The platform would admit it, execute it, and only then compute how
// bad it was.

func riskRule(measure string, max int64) *compliancepb.Rule {
	return &compliancepb.Rule{
		RuleId: "r-1",
		Type:   compliancepb.RuleType_RULE_TYPE_RISK_MEASURE,
		Params: &compliancepb.Rule_RiskMeasure{
			RiskMeasure: &compliancepb.RiskMeasureLimit{
				MeasureName: measure,
				MaxValue:    &commonpb.Decimal{Coefficient: max},
			},
		},
	}
}

func bookWithRisk(f func(string) (*big.Rat, bool)) *Candidate {
	return &Candidate{Book: &Book{PortfolioID: "fund-alpha", BaseCurrency: "USD", Risk: f}}
}

// A PORTFOLIO INSIDE ITS LIMIT PASSES. Required as the companion to every
// refusal below: a rule that refuses everything is a trading outage wearing the
// shape of a control.
func TestRiskLimitRule_WithinTheLimitPasses(t *testing.T) {
	c := bookWithRisk(func(string) (*big.Rat, bool) { return big.NewRat(90, 1), true })

	if v := RiskLimitRule(c, riskRule("VaR99", 100)); v != nil {
		t.Fatalf("a VaR of 90 against a limit of 100 was refused: %s", v.GetMessage())
	}
}

// OVER THE LIMIT IS REFUSED, and the refusal carries both numbers.
//
// An operator reading "over its risk limit" without the observed value and the
// ceiling cannot tell a marginal breach from a catastrophic one, and cannot tell
// whether the limit or the book is what needs attention.
func TestRiskLimitRule_OverTheLimitIsRefusedWithBothNumbers(t *testing.T) {
	c := bookWithRisk(func(string) (*big.Rat, bool) { return big.NewRat(150, 1), true })

	v := RiskLimitRule(c, riskRule("VaR99", 100))
	if v == nil {
		t.Fatal("a VaR of 150 against a limit of 100 was ADMITTED — this is the gap #438 exists " +
			"for: the order is inside every other mandate rule and still breaches risk")
	}
	if !strings.HasPrefix(v.GetEvidence()["observed"], "150") ||
		!strings.HasPrefix(v.GetEvidence()["limit"], "100") {
		t.Errorf("evidence = %v, want observed 150 and limit 100 — an operator cannot tell a "+
			"marginal breach from a catastrophic one without both", v.GetEvidence())
	}
	if v.GetEvidence()["measure"] != "VaR99" {
		t.Errorf("the refusal does not name the measure: %v", v.GetEvidence())
	}
}

// EXACTLY AT THE LIMIT PASSES. A ceiling is a ceiling: "must not exceed" and
// "must be strictly below" differ by one order at the boundary, and the schema
// says exceed.
func TestRiskLimitRule_AtTheLimitPasses(t *testing.T) {
	c := bookWithRisk(func(string) (*big.Rat, bool) { return big.NewRat(100, 1), true })

	if v := RiskLimitRule(c, riskRule("VaR99", 100)); v != nil {
		t.Fatalf("a VaR exactly at the limit was refused: %s", v.GetMessage())
	}
}

// AN UNKNOWN MEASURE FAILS CLOSED.
//
// Admitting an order because the platform could not establish its risk is a
// control that reports success. It is worse here than for cash: a portfolio whose
// risk cannot be computed is precisely the one nobody should be adding to.
func TestRiskLimitRule_UnknownMeasureIsRefusedNotAdmitted(t *testing.T) {
	c := bookWithRisk(func(string) (*big.Rat, bool) { return nil, false })

	v := RiskLimitRule(c, riskRule("VaR99", 100))
	if v == nil {
		t.Fatal("an order was ADMITTED against a portfolio whose VaR is unknown, while its " +
			"mandate declares a VaR limit — a control that reports success")
	}
	if v.GetEvidence()["risk"] != "unknown" {
		t.Errorf("evidence = %v, want risk=unknown so an operator can tell this from a breach",
			v.GetEvidence())
	}
}

// NO RISK SOURCE AT ALL IS ALSO A REFUSAL, and distinguishable from an unknown
// measure. A deployment that has not wired risk and declares a VaR ceiling must
// not read as compliant — but its operator needs to see "unavailable", not
// "unknown", because the fix is different.
func TestRiskLimitRule_NoSourceIsRefusedAndDistinguishable(t *testing.T) {
	c := &Candidate{Book: &Book{PortfolioID: "fund-alpha"}} // Risk nil

	v := RiskLimitRule(c, riskRule("VaR99", 100))
	if v == nil {
		t.Fatal("a declared risk limit passed with no risk source wired at all")
	}
	if v.GetEvidence()["risk"] != "unavailable" {
		t.Errorf("evidence = %v, want risk=unavailable — a missing wiring and a stale measure "+
			"need different fixes", v.GetEvidence())
	}
}

// A LIMIT NAMING NO MEASURE IS A REFUSAL. The mandate declared a control and did
// not say on what; treating that as satisfied would let an empty rule read as an
// enforced one.
func TestRiskLimitRule_ALimitNamingNoMeasureIsRefused(t *testing.T) {
	c := bookWithRisk(func(string) (*big.Rat, bool) { return big.NewRat(1, 1), true })

	if v := RiskLimitRule(c, riskRule("", 100)); v == nil {
		t.Fatal("a risk limit naming no measure passed — an empty control read as an enforced one")
	}
}

// A LIMIT ON A MEASURE THE ENGINE DOES NOT PRODUCE IS REFUSED, by the same path
// as any other unknown.
//
// measure_name is the engine's own name rather than an enum mirrored into the
// schema — deliberately, so the quant library can add measures without a schema
// change. The cost of that choice is that a mandate CAN name something nothing
// computes, and the honest answer to "your limit refers to a number that does not
// exist" is not to let the order through.
func TestRiskLimitRule_AMeasureNobodyComputesIsRefused(t *testing.T) {
	c := bookWithRisk(func(name string) (*big.Rat, bool) {
		if name == "VaR99" {
			return big.NewRat(10, 1), true
		}
		return nil, false // the engine produces VaR99 and nothing else
	})

	if v := RiskLimitRule(c, riskRule("ExpectedShortfall97", 100)); v == nil {
		t.Fatal("a limit on a measure the engine does not compute was treated as satisfied")
	}
}

// THE RULE IS EXACT. A risk limit decided by a rounding error is a limit decided
// by the implementation rather than by the mandate.
func TestRiskLimitRule_IsExactAtTheBoundary(t *testing.T) {
	// 100 + 10^-20, against a limit of 100. THE VALUE IS CHOSEN SO A FLOAT CANNOT
	// SEE IT: float64(100 + 1e-20) is exactly 100.0, so a comparison that left the
	// exact domain anywhere on the path would ADMIT this order. big.Rat refuses it.
	over := new(big.Rat).Add(big.NewRat(100, 1), new(big.Rat).SetFrac(
		big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil)))
	c := bookWithRisk(func(string) (*big.Rat, bool) { return over, true })

	if v := RiskLimitRule(c, riskRule("VaR99", 100)); v == nil {
		t.Fatal("a VaR of 100+1e-20 against a limit of 100 PASSED — a float64 rounds that to " +
			"exactly 100, so the comparison left the exact domain and the limit is decided by " +
			"the implementation rather than by the mandate")
	}
}

// THE WRONG PARAMS ARE A MISMATCH, not a pass — the same contract every other
// rule here keeps.
func TestRiskLimitRule_WrongParamsAreAMismatch(t *testing.T) {
	c := bookWithRisk(func(string) (*big.Rat, bool) { return big.NewRat(1, 1), true })
	wrong := &compliancepb.Rule{RuleId: "r-1", Type: compliancepb.RuleType_RULE_TYPE_RISK_MEASURE}

	if v := RiskLimitRule(c, wrong); v == nil {
		t.Fatal("a risk rule carrying no risk params passed")
	}
}

// AND IT IS REGISTERED. A rule the engine cannot dispatch is a rule that never
// runs, which is the shape #74's unwind decider sat in for months.
func TestRiskLimitRule_IsRegisteredInTheDefaultRegistry(t *testing.T) {
	r := DefaultRegistry()
	if _, ok := r.funcs[compliancepb.RuleType_RULE_TYPE_RISK_MEASURE]; !ok {
		t.Fatal("RULE_TYPE_RISK_MEASURE has no evaluator in the default registry — a mandate " +
			"declaring a risk limit would be dispatched to nothing")
	}
}
