package stress

import (
	"math"
	"testing"
)

// A MACRO FACTOR THE SCENARIO DID NOT CARRY IS NOT A ZERO SHOCK (#623).
//
// Expand reads s.Shocks[macro], and a missing key in a Go map is 0. So a beta
// model expecting {gdp, unemployment, equity, rates, credit_spread} fed a
// scenario carrying only {equity} still emitted a full shock vector for every
// asset class — with four transmission channels contributing nothing, and no
// count, no flag and no error to say so.
//
// The two states that collapses are opposites:
//
//	the scenario says this factor does not move   0 is the right shock
//	the scenario never mentioned this factor      0 is an absence of data
//
// It matters most where it is least visible. That vector feeds ReverseStress,
// whose answer — how bad conditions must get before a breach — is then
// UNDERSTATED, in the direction the filer benefits from, on a number that goes
// into a regulatory submission.
//
// EXPAND STILL RETURNS THE VECTOR RATHER THAN AN ERROR, and that is deliberate.
// A single-factor scenario is a legitimate thing to run; what is not legitimate
// is being unable to tell one from a five-factor scenario that lost four. So the
// coverage travels WITH the number, the way #618 made a climate metric carry the
// share of the book it measured.

func fiveFactorModel() ExpansionModel {
	return ExpansionModel{Beta: map[string]map[string]float64{
		"Equity": {"gdp": 1.2, "equity": 0.9, "rates": -0.4, "credit_spread": -0.6, "unemployment": -0.3},
		"Credit": {"gdp": 0.5, "credit_spread": -1.1},
	}}
}

func TestCoverageNamesTheFactorsTheScenarioNeverCarried(t *testing.T) {
	m := fiveFactorModel()
	_, cov := m.Expand(MacroScenario{Name: "equity-only", Severity: 1, Shocks: map[string]float64{"equity": -0.3}})

	if cov.Complete() {
		t.Fatal("a scenario carrying one of five factors reported complete coverage")
	}
	if len(cov.Missing) != 4 {
		t.Fatalf("missing = %v, want the four factors the scenario never carried", cov.Missing)
	}
	// Sorted, so an operator reading two runs can diff them.
	want := []string{"credit_spread", "gdp", "rates", "unemployment"}
	for i, f := range want {
		if cov.Missing[i] != f {
			t.Fatalf("missing = %v, want %v (sorted)", cov.Missing, want)
		}
	}
	if len(cov.Supplied) != 1 || cov.Supplied[0] != "equity" {
		t.Fatalf("supplied = %v, want [equity]", cov.Supplied)
	}
	if got := cov.Fraction(); math.Abs(got-0.2) > 1e-9 {
		t.Fatalf("fraction = %v, want 0.2 (1 of 5)", got)
	}
}

// A FULLY SUPPLIED SCENARIO SAYS SO, and that is what makes the incomplete one
// mean something.
func TestAFullyCoveredScenarioReportsComplete(t *testing.T) {
	m := fiveFactorModel()
	_, cov := m.Expand(MacroScenario{Name: "full", Severity: 1, Shocks: map[string]float64{
		"gdp": -0.04, "equity": -0.3, "rates": -0.02, "credit_spread": 0.03, "unemployment": 0.02,
	}})

	if !cov.Complete() {
		t.Fatalf("a scenario carrying every factor reported incomplete: missing=%v", cov.Missing)
	}
	if cov.Fraction() != 1 {
		t.Fatalf("fraction = %v, want 1", cov.Fraction())
	}
}

// AN EXPLICIT ZERO IS SUPPLIED DATA, NOT AN ABSENCE. This is the distinction the
// whole record exists to make: a scenario stating "rates do not move" has
// supplied that factor, and must not read as having lost it.
func TestAnExplicitZeroCountsAsSupplied(t *testing.T) {
	m := ExpansionModel{Beta: map[string]map[string]float64{"Equity": {"equity": 1, "rates": -0.5}}}
	_, cov := m.Expand(MacroScenario{Name: "flat-rates", Severity: 1, Shocks: map[string]float64{
		"equity": -0.2, "rates": 0,
	}})

	if !cov.Complete() {
		t.Fatalf("an explicit zero was treated as a missing factor: missing=%v", cov.Missing)
	}
}

// A FACTOR NO ASSET CLASS RESPONDS TO IS NOT REQUIRED. A beta of zero means the
// channel carries nothing, so its absence changes no number and reporting it
// missing would train a reader to ignore the record.
func TestAFactorWithNoBetaIsNotRequired(t *testing.T) {
	m := ExpansionModel{Beta: map[string]map[string]float64{
		"Equity": {"equity": 1, "sentiment": 0},
	}}
	_, cov := m.Expand(MacroScenario{Name: "e", Severity: 1, Shocks: map[string]float64{"equity": -0.2}})

	if !cov.Complete() {
		t.Fatalf("a zero-beta factor was reported as required: missing=%v", cov.Missing)
	}
}

// THE SHOCKS THEMSELVES ARE UNCHANGED. The repair reports; it must not quietly
// alter a number anyone already relies on.
func TestExpandStillProducesTheSameShocks(t *testing.T) {
	m := ExpansionModel{Beta: map[string]map[string]float64{"Equity": {"equity": 1.3}}}
	out, _ := m.Expand(MacroScenario{Name: "e", Severity: 2, Shocks: map[string]float64{"equity": -0.2}})

	if got := out["Equity"]; math.Abs(got-(-0.52)) > 1e-9 {
		t.Fatalf("Equity shock = %v, want -0.52", got)
	}
}
