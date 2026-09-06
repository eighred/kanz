package riskview

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/compliance"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// A DELTA MANDATE IS A PERMANENT REFUSAL ON EVERY DEPLOYMENT, AND THAT IS THE
// DECIDED BEHAVIOUR (#1055).
//
// # What changed under operators without being announced
//
// compute.Delta returns the signed base-currency sum — NetExposure under a
// second name — and it is registered in compute.DefaultRegistry, which every
// engine builds. compute.RegisterGreeks is the only thing that overwrites it and
// has no production caller anywhere in this estate, so the placeholder is what
// EVERY pod serves; unlike the VaR placeholder there is no DSN that switches it
// off. Before #1037 that number folded silently and a mandate naming Delta gated
// on net exposure. After #1037 riskview refuses the declared placeholder, the
// measure is UNKNOWN, and RiskLimitRule fails closed — so the mandate refuses
// every order instead of checking one.
//
// That direction is right: a control that refuses is safer than one that passes
// on a number nobody can vouch for. It is also a live change in admission
// behaviour for any tenant holding a Delta limit, and nothing in the platform
// announced it. This test is the executable half of the statement, and
// StateGreekModelPosture in services/risk-engine is the operational half.
//
// # Why the assertion runs the real rule rather than stopping at the fold
//
// riskview_test already proves the fold declines the measure. What an operator
// experiences is one layer further on, and the two are separable: a rule that
// treated an unknown measure as unlimited would leave this fold correct and the
// gate wide open, which is #261's shape. The refusal has to be asserted where
// the order is decided.
func TestDeltaMandateIsRefusedNotGatedOnNetExposure(t *testing.T) {
	view := New(WithClock(func() time.Time { return t0 }))

	// Exactly what a risk-engine pod announces today: Delta, a real number, over
	// the whole book, declaring the net-exposure placeholder as its model.
	if err := view.Handle(context.Background(), nil,
		measuresWithProvenance(t, "fund-alpha", t0, "Delta", 900, "net_exposure_placeholder")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	candidate := &compliance.Candidate{Book: &compliance.Book{
		PortfolioID:  "fund-alpha",
		BaseCurrency: "USD",
		Risk:         func(name string) (*big.Rat, bool) { return view.Measure("fund-alpha", name) },
	}}
	rule := &compliancepb.Rule{
		RuleId: "delta-limit",
		Type:   compliancepb.RuleType_RULE_TYPE_RISK_MEASURE,
		Params: &compliancepb.Rule_RiskMeasure{RiskMeasure: &compliancepb.RiskMeasureLimit{
			MeasureName: "Delta",
			// A ceiling the announced 900 sits WELL INSIDE. If the placeholder were
			// folded this rule would pass, so a passing rule here is not "the limit
			// held" — it is the placeholder gating admission again.
			MaxValue: &commonpb.Decimal{Coefficient: 100000},
		}},
	}

	v := compliance.RiskLimitRule(candidate, rule)
	if v == nil {
		t.Fatal("a Delta mandate PASSED against a net-exposure placeholder — the platform is " +
			"gating order admission on a number that is NetExposure wearing a Greek's name, " +
			"and no deployment can make it a real delta because RegisterGreeks has no caller (#1055)")
	}
	if got := v.GetEvidence()["risk"]; got != "unknown" {
		t.Errorf("the Delta refusal reports risk=%q, want %q — the mandate must refuse because "+
			"the measure is UNKNOWN, not because the book breached a limit it was never "+
			"checked against", got, "unknown")
	}
	if got := v.GetEvidence()["measure"]; got != "Delta" {
		t.Errorf("the refusal names measure %q, want Delta — an operator reading it cannot "+
			"tell which limit stopped the order", got)
	}
}

// AND A REAL DELTA WOULD STILL GATE. The refusal above must be a property of the
// MODEL, not of the measure's name: the day RegisterGreeks is wired, Delta
// declares option_pricing_greeks and a Delta mandate must go back to checking a
// number rather than refusing. Without this arm the fix above is
// indistinguishable from blacklisting the name.
func TestDeltaMandateGatesOnceTheGreekIsModelDerived(t *testing.T) {
	view := New(WithClock(func() time.Time { return t0 }))
	if err := view.Handle(context.Background(), nil,
		measuresWithProvenance(t, "fund-alpha", t0, "Delta", 900, "option_pricing_greeks")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	candidate := &compliance.Candidate{Book: &compliance.Book{
		PortfolioID:  "fund-alpha",
		BaseCurrency: "USD",
		Risk:         func(name string) (*big.Rat, bool) { return view.Measure("fund-alpha", name) },
	}}
	within := &compliancepb.Rule{
		RuleId: "delta-limit",
		Type:   compliancepb.RuleType_RULE_TYPE_RISK_MEASURE,
		Params: &compliancepb.Rule_RiskMeasure{RiskMeasure: &compliancepb.RiskMeasureLimit{
			MeasureName: "Delta",
			MaxValue:    &commonpb.Decimal{Coefficient: 100000},
		}},
	}
	if v := compliance.RiskLimitRule(candidate, within); v != nil {
		t.Fatalf("a pricing-derived Delta inside its limit was refused (%s) — the placeholder "+
			"refusal has become a permanent blacklist on the measure name, which would make "+
			"wiring RegisterGreeks a no-op for admission", v.GetMessage())
	}
}
