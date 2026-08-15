package benchmarks

import (
	"math"

	"github.com/eighred/kanz/internal/regulatory/stress"
	"github.com/eighred/kanz/internal/validation"
)

// THE REG-01c FIRMWIDE STRESS FRAMEWORK (#471).
//
// # What this validates, precisely
//
// Two things, and they are graded differently because they are different kinds of
// thing.
//
// THE EXPANSION is a linear factor model: assetClassShock = Σ beta·macroShock·
// severity. Every property it has is exact — homogeneity in severity, additivity
// across factors, zero in / zero out — so these carry 1e-12 tolerances and no
// argument. A linear map that fails any of them is not an approximation of a
// linear map; it is a different function.
//
// THE REVERSE STRESS is a bisection over an injected loss function, and its
// contract is sharper than "returns a number": it returns the SMALLEST severity
// that breaches. That is testable exactly — the loss at the answer must reach the
// threshold and the loss just below it must not — and it is worth testing that
// way, because a bisection that converges to the wrong side of the boundary
// returns a plausible severity that understates how much stress the book can
// take.
//
// # Why there is no published vector here, and what stands in for one
//
// CCAR and DFAST publish SCENARIOS — paths for GDP, unemployment, equity indices
// — not the transmission betas, which are each firm's own model. So there is no
// external number to match, and the substitute is the set of algebraic identities
// the transmission must satisfy whatever the betas are. That is weaker than a
// published vector and it is what is available; the cases say so in their names
// (definitional, not published).
//
// # The one that would be silent
//
// ReverseStress returns (maxSeverity, false) when nothing in range breaches. A
// caller reading only the severity sees a large number and concludes the book
// breaks under severe stress — the exact opposite of what happened. The ok flag
// is the whole answer in that case, and a case below pins that the severity
// returned alongside ok=false is NOT zero, because zero would read as "breaches
// immediately" and is the more dangerous misreading of the two.

// AnalyticStressFramework is the macro-scenario expansion and reverse stress.
const AnalyticStressFramework = "stress_framework"

// A two-asset-class transmission with mixed signs: equities fall when GDP falls,
// and rally when rates fall; credit widens on both.
var stressModel = stress.ExpansionModel{Beta: map[string]map[string]float64{
	"EQUITY": {"gdp": 2.5, "rates": -1.5},
	"CREDIT": {"gdp": -1.0, "rates": 0.5},
}}

var baseScenario = stress.MacroScenario{
	Name:   "severely-adverse",
	Shocks: map[string]float64{"gdp": -0.04, "rates": -0.02},
}

// StressFramework grades the macro expansion and the reverse-stress search.
func StressFramework() []validation.Case {
	// Severity 1, computed by hand: EQUITY = 2.5·(−0.04) + (−1.5)·(−0.02) = −0.07;
	// CREDIT = (−1.0)·(−0.04) + 0.5·(−0.02) = 0.03.
	base := stressModel.Expand(withSeverity(baseScenario, 1))

	cases := []validation.Case{
		{
			Name: "macro_expansion_matches_the_hand_computed_transmission_definitional",
			Got:  base["EQUITY"], Want: -0.07, Tolerance: 1e-12,
		},
		{
			// THE SIGNS MUST NOT COLLAPSE. Both macro shocks are negative here and
			// the two asset classes move in OPPOSITE directions, so a transmission
			// that dropped a beta's sign would still produce plausible magnitudes.
			Name: "macro_expansion_preserves_opposing_signs_across_asset_classes_definitional",
			Got:  base["CREDIT"], Want: 0.03, Tolerance: 1e-12,
		},
		{
			// HOMOGENEITY IN SEVERITY: doubling the severity doubles every shock.
			// This is the property reverse stress WALKS — if severity were not a
			// clean multiplier the bisection would be searching a dimension that
			// does not mean what its name says.
			Name: "expansion_is_homogeneous_in_severity_definitional",
			Got:  stressModel.Expand(withSeverity(baseScenario, 2))["EQUITY"],
			Want: 2 * base["EQUITY"], Tolerance: 1e-12,
		},
		{
			// AND AT A FRACTIONAL SEVERITY, so the case above is not satisfied by an
			// implementation that only handles integers.
			Name: "expansion_scales_at_fractional_severity_definitional",
			Got:  stressModel.Expand(withSeverity(baseScenario, 0.25))["CREDIT"],
			Want: 0.25 * base["CREDIT"], Tolerance: 1e-12,
		},
		{
			// ADDITIVITY ACROSS FACTORS: the shock from {gdp, rates} together equals
			// the sum of each alone. The defining property of a linear transmission,
			// and the one that fails if a factor is applied twice or dropped.
			Name: "expansion_is_additive_across_macro_factors_definitional",
			Got: stressModel.Expand(scenarioWith("gdp", -0.04))["EQUITY"] +
				stressModel.Expand(scenarioWith("rates", -0.02))["EQUITY"],
			Want: base["EQUITY"], Tolerance: 1e-12,
		},
		{
			// NO SHOCK IS NO SHOCK. A framework that produced movement from an
			// unstressed scenario would report a loss under the baseline, which is
			// the number every stress result is measured against.
			Name: "an_unshocked_scenario_expands_to_zero_definitional",
			Got:  math.Abs(stressModel.Expand(stress.MacroScenario{Name: "base", Severity: 1})["EQUITY"]),
			Want: 0, Tolerance: 1e-12,
		},
		{
			// A ZERO SEVERITY IS READ AS ONE, not as zero — MacroScenario.Severity's
			// documented default. Pinned because the opposite reading is silent: an
			// unset severity would expand every scenario to nothing and report a
			// firm that is never stressed.
			Name: "an_unset_severity_defaults_to_one_definitional",
			Got:  stressModel.Expand(baseScenario)["EQUITY"], Want: -0.07, Tolerance: 1e-12,
		},
		{
			Name: "asset_classes_are_returned_in_stable_order_definitional",
			Got:  boolAsFloat(equalStrings(stressModel.AssetClasses(), []string{"CREDIT", "EQUITY"})),
			Want: 1, Tolerance: 0.5,
		},
	}

	// ===== REVERSE STRESS =====
	//
	// A loss that is linear in severity, so the breaching severity has a closed
	// form: loss(s) = 200·s reaches 50 at s = 0.25.
	linear := func(s float64) float64 { return 200 * s }

	sev, ok := stress.ReverseStress(linear, 50, 4)
	cases = append(cases,
		validation.Case{
			Name: "reverse_stress_finds_the_closed_form_breaching_severity_definitional",
			Got:  boolFloat(ok, sev), Want: 0.25, Tolerance: 1e-8,
		},
		validation.Case{
			// THE ANSWER IS THE SMALLEST BREACHING SEVERITY, which is a stronger
			// statement than the number above: the loss AT it reaches the threshold
			// and the loss just below it does not. A bisection converging to the
			// wrong side of the boundary returns a plausible severity that
			// overstates how much stress the book absorbs.
			Name: "the_returned_severity_is_the_smallest_that_breaches_definitional",
			Got:  boolAsFloat(ok && linear(sev) >= 50 && linear(sev-1e-6) < 50),
			Want: 1, Tolerance: 0.5,
		},
		validation.Case{
			// ALREADY BREACHED AT ZERO. A book over its limit before any stress is
			// applied is a real and urgent state, and reporting the smallest
			// breaching severity as anything above zero would hide it.
			Name: "a_book_already_over_its_limit_breaches_at_zero_severity_definitional",
			Got:  boolFloat(secondOf(stress.ReverseStress(func(float64) float64 { return 100 }, 50, 4)), firstOf(stress.ReverseStress(func(float64) float64 { return 100 }, 50, 4))),
			Want: 0, Tolerance: 1e-12,
		},
	)

	// NOTHING IN RANGE BREACHES, and the flag is the entire answer.
	noBreachSev, noBreachOK := stress.ReverseStress(func(s float64) float64 { return s }, 1e6, 4)
	cases = append(cases,
		validation.Case{
			Name: "a_scenario_that_cannot_breach_in_range_reports_not_ok_definitional",
			Got:  boolAsFloat(!noBreachOK), Want: 1, Tolerance: 0.5,
		},
		validation.Case{
			// AND THE SEVERITY IT RETURNS IS maxSeverity, NOT ZERO. A caller that
			// reads the number and ignores the flag sees "breaches at 4" — wrong,
			// but conservative. Zero would read as "breaches immediately", which is
			// the dangerous direction, so which value comes back matters.
			Name: "the_severity_returned_alongside_not_ok_is_the_search_ceiling_definitional",
			Got:  noBreachSev, Want: 4, Tolerance: 1e-12,
		},
		validation.Case{
			// MONOTONICITY IS ASSUMED BY THE SEARCH, so a steeper loss must breach
			// no later than a shallower one. It catches a bisection whose comparison
			// is inverted, which otherwise returns a number from the right interval.
			Name: "a_steeper_loss_breaches_no_later_than_a_shallower_one_definitional",
			Got: boolAsFloat(firstOf(stress.ReverseStress(func(s float64) float64 { return 400 * s }, 50, 4)) <=
				firstOf(stress.ReverseStress(func(s float64) float64 { return 200 * s }, 50, 4))),
			Want: 1, Tolerance: 0.5,
		},
	)

	return cases
}

// withSeverity copies a scenario at a given severity.
func withSeverity(s stress.MacroScenario, sev float64) stress.MacroScenario {
	return stress.MacroScenario{Name: s.Name, Shocks: s.Shocks, Severity: sev}
}

// scenarioWith builds a single-factor scenario at severity 1.
func scenarioWith(factor string, shock float64) stress.MacroScenario {
	return stress.MacroScenario{Name: factor, Shocks: map[string]float64{factor: shock}, Severity: 1}
}

func firstOf(sev float64, _ bool) float64 { return sev }
func secondOf(_ float64, ok bool) bool    { return ok }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
