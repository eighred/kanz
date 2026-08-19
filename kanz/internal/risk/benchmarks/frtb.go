package benchmarks

import (
	"math"

	"github.com/eighred/kanz/internal/regulatory/frtb"
	"github.com/eighred/kanz/internal/validation"
)

// THE REG-01b FRTB SENSITIVITIES-BASED METHOD (#471, coverage 10/12 -> 11/12).
//
// # Why this analytic sat at zero, and what changed
//
// frtb_sa was one of the two entries #471 left as a bare string in Inventory()
// with no case set, on the argument that the supervisory RISK WEIGHTS are not
// derivable — params.go ships representative magnitudes rather than the published
// MAR21 tables, so grading a capital number against a figure computed here would
// be this codebase checking its own arithmetic.
//
// That argument covers the CALIBRATION and not the AGGREGATION. The algebra that
// turns weighted sensitivities into a capital charge is fully specified in public
// text — MAR21.4 for the bucket and cross-bucket sums, MAR21.6 for the three
// correlation scenarios and the max over them — and it is where the engine's own
// logic lives. So these cases grade the aggregation and say nothing about the
// weights, which is the honest split rather than a 12/12 bought by pretending
// otherwise. isda_simm stays at zero for exactly the reason frtb_sa no longer
// does: its aggregation is member-licensed too.
//
// # DEFINITIONAL, on the bar stress_framework already accepted
//
// Every case below is named `_definitional`, not `_published`. There is no vendor
// vector here: the expected values are the algebra's own identities, computed
// from the INPUTS by hand or forced by a limiting correlation, never read out of
// an engine output. That is weaker than Hull's worked example and stronger than
// nothing, and the names say which so a reader of the report can tell.
//
// # The parameter that has to be varied, and why
//
// #471's expected-shortfall finding established the rule: vary whatever the
// estimator discretises on, because a single-point check says nothing about its
// neighbours. Here the discretisation is the SCENARIO — ρ and γ are each mapped
// through max(2ρ−1, 0.75ρ) and min(1.25ρ, 1), and those two branches select on
// the value of ρ. So the low-correlation cases run at ρ=0.2 where the 0.75ρ FLOOR
// binds and at ρ=0.9 where 2ρ−1 binds. An implementation that had only the
// 2ρ−1 arm reproduces the second and returns a NEGATIVE correlation for the
// first, which un-nets an offsetting book into more capital and nets an aligned
// one into less. One of those directions is a filing that is too small.
//
// # The case that found something
//
// `two_offsetting_buckets_are_not_free_definitional` is here because the
// cross-bucket radicand could go negative and the engine answered ZERO — no
// capital required for a book with real gross risk. MAR21.4(5) prescribes
// recomputing with Sb capped to ±Kb instead, which the same package's curvature.go
// had always done. See sbm.go. The case is written against the published
// alternative specification, so it fails on the old behaviour rather than
// recording it.

// AnalyticFRTBSA is the FRTB standardised approach — the SBM delta/vega
// aggregation. It does NOT cover the supervisory risk weights, which are
// representative rather than published; see this file's header.
const AnalyticFRTBSA = "frtb_sa"

// A single risk class with a flat 1.0 risk weight, so every case below reads in
// units of the sensitivity itself and a wrong aggregation cannot hide behind a
// weight. The weight's own effect is graded separately.
const frtbClass = "TEST"

func frtbParams(rho, gamma float64) frtb.ClassParams {
	return frtb.ClassParams{
		RiskWeight: map[string]float64{"1": 1, "2": 1},
		IntraCorr:  rho,
		InterCorr:  gamma,
	}
}

// frtbSens builds one sensitivity row.
func frtbSens(bucket, factor string, amount float64) frtb.Sensitivity {
	return frtb.Sensitivity{RiskClass: frtbClass, Bucket: bucket, Factor: factor, Amount: amount}
}

// FRTBSA grades the MAR21 sensitivities-based aggregation.
func FRTBSA() []validation.Case {
	cases := frtbBucketAggregationCases()
	cases = append(cases, frtbScenarioCases()...)
	cases = append(cases, frtbCrossBucketCases()...)
	cases = append(cases, frtbStructuralCases()...)
	return cases
}

// ===== MAR21.4(3): the within-bucket aggregation =====

func frtbBucketAggregationCases() []validation.Case {
	// One bucket, two aligned factors of 30 and 40.
	aligned := []frtb.Sensitivity{frtbSens("1", "X", 30), frtbSens("1", "Y", 40)}
	// One bucket, two offsetting factors.
	offsetting := []frtb.Sensitivity{frtbSens("1", "X", 30), frtbSens("1", "Y", -40)}

	return []validation.Case{
		{
			// A ONE-FACTOR BUCKET IS ITS OWN WEIGHTED SENSITIVITY, whatever ρ is —
			// there is no cross term to correlate. Kb = |WS| = 30. It is the case
			// that fails if the cross term is applied to the diagonal.
			Name: "a_single_factor_bucket_is_its_weighted_sensitivity_definitional",
			Got:  frtb.ChargeForScenario([]frtb.Sensitivity{frtbSens("1", "X", 30)}, frtbParams(0.7, 0.7), frtb.Medium),
			Want: 30, Tolerance: 1e-12,
		},
		{
			// ρ = 0 IS THE EUCLIDEAN NORM: √(30² + 40²) = 50. Zero correlation is
			// full netting benefit, and it is the only ρ at which the answer does
			// not depend on the signs.
			Name: "zero_intra_bucket_correlation_is_the_euclidean_norm_definitional",
			Got:  frtb.ChargeForScenario(aligned, frtbParams(0, 0), frtb.Medium),
			Want: 50, Tolerance: 1e-12,
		},
		{
			// AND THE SAME 50 WHEN THE FACTORS OFFSET, at ρ=0. Signs are invisible
			// to the Euclidean norm, so this pins that ρ=0 really is ρ=0 rather
			// than a small correlation that happens to be close.
			Name: "at_zero_correlation_offsetting_factors_aggregate_identically_definitional",
			Got:  frtb.ChargeForScenario(offsetting, frtbParams(0, 0), frtb.Medium),
			Want: 50, Tolerance: 1e-12,
		},
		{
			// ρ = 1 IS THE PLAIN SUM: Kb = |ΣWS| = 70. Reached through the HIGH
			// scenario at ρ=0.8, where min(1.25·0.8, 1) = 1 exactly — so this case
			// grades the 1.25 multiplier and the cap at the same time.
			Name: "perfect_intra_bucket_correlation_is_the_plain_sum_definitional",
			Got:  frtb.ChargeForScenario(aligned, frtbParams(0.8, 0.8), frtb.High),
			Want: 70, Tolerance: 1e-12,
		},
		{
			// AND AT ρ = 1 AN OFFSETTING BUCKET NETS TO |30 − 40| = 10. Perfect
			// correlation is the maximum netting benefit for opposed positions and
			// the minimum for aligned ones — the same ρ, opposite directions, which
			// is what a dropped sign in the cross term destroys.
			Name: "at_perfect_correlation_offsetting_factors_net_to_their_difference_definitional",
			Got:  frtb.ChargeForScenario(offsetting, frtbParams(0.8, 0.8), frtb.High),
			Want: 10, Tolerance: 1e-12,
		},
		{
			// THE HAND-COMPUTED INTERIOR POINT, so the two limits above are not
			// satisfied by an implementation that only handles ρ ∈ {0,1}:
			// ρ=0.5 ⇒ √(900 + 1600 + 0.5·2·30·40) = √(2500 + 1200) = √3700.
			Name: "an_interior_correlation_matches_the_hand_computed_root_definitional",
			Got:  frtb.ChargeForScenario(aligned, frtbParams(0.5, 0.5), frtb.Medium),
			Want: math.Sqrt(3700), Tolerance: 1e-9,
		},
		{
			// SENSITIVITIES TO THE SAME FACTOR NET BEFORE WEIGHTING. MAR21.4(1)
			// nets at the risk-factor level, so two rows are one factor and must
			// not be aggregated as two correlated ones — which would report
			// √(WS₁²+WS₂²+2ρ·WS₁WS₂) > |WS₁+WS₂| for any ρ < 1 and overstate every
			// book fed row-per-trade rather than row-per-factor.
			Name: "rows_on_the_same_risk_factor_net_before_weighting_definitional",
			Got: frtb.ChargeForScenario([]frtb.Sensitivity{
				frtbSens("1", "X", 100), frtbSens("1", "X", -40),
			}, frtbParams(0.5, 0.5), frtb.Medium),
			Want: 60, Tolerance: 1e-12,
		},
	}
}

// ===== MAR21.6: the three correlation scenarios and the max over them =====

func frtbScenarioCases() []validation.Case {
	aligned := []frtb.Sensitivity{frtbSens("1", "X", 30), frtbSens("1", "Y", 40)}
	offsetting := []frtb.Sensitivity{frtbSens("1", "X", 30), frtbSens("1", "Y", -40)}

	// ρ = 0.2: 2ρ−1 = −0.6 and 0.75ρ = 0.15, so the FLOOR binds and the low
	// scenario correlation is 0.15 — positive. An implementation carrying only the
	// 2ρ−1 arm would use −0.6 here.
	lowFloorAligned := math.Sqrt(2500 + 0.15*2*30*40)
	// ρ = 0.9: 2ρ−1 = 0.8 and 0.75ρ = 0.675, so 2ρ−1 binds.
	lowLinearAligned := math.Sqrt(2500 + 0.8*2*30*40)

	return []validation.Case{
		{
			// THE 0.75ρ FLOOR, at the ρ where it is the binding arm. The control
			// case for the branch below: one value of ρ cannot exercise both.
			Name: "the_low_correlation_scenario_uses_the_three_quarters_floor_definitional",
			Got:  frtb.ChargeForScenario(aligned, frtbParams(0.2, 0.2), frtb.Low),
			Want: lowFloorAligned, Tolerance: 1e-9,
		},
		{
			// AND THE 2ρ−1 ARM, at the ρ where THAT binds. Both are needed: a
			// max() with one arm missing reproduces the other arm's cases exactly.
			Name: "the_low_correlation_scenario_uses_two_rho_minus_one_when_it_binds_definitional",
			Got:  frtb.ChargeForScenario(aligned, frtbParams(0.9, 0.9), frtb.Low),
			Want: lowLinearAligned, Tolerance: 1e-9,
		},
		{
			// THE 1.25 MULTIPLIER BELOW THE CAP: ρ=0.4 ⇒ 0.5, not 0.4 and not 1.
			// The `perfect correlation` case above runs at the cap, so without this
			// an implementation returning min(1, ρ) would pass every high-scenario
			// case here.
			Name: "the_high_correlation_scenario_scales_by_one_and_a_quarter_definitional",
			Got:  frtb.ChargeForScenario(aligned, frtbParams(0.4, 0.4), frtb.High),
			Want: math.Sqrt(2500 + 0.5*2*30*40), Tolerance: 1e-9,
		},
		{
			// THE MEDIUM SCENARIO IS ρ UNCHANGED. Pinned because "medium" is the
			// only one of the three that is a no-op, and a scaling accidentally
			// applied to it moves every charge without changing any relative order.
			Name: "the_medium_scenario_leaves_the_correlation_alone_definitional",
			Got:  frtb.ChargeForScenario(aligned, frtbParams(0.6, 0.6), frtb.Medium),
			Want: math.Sqrt(2500 + 0.6*2*30*40), Tolerance: 1e-9,
		},
		{
			// AN OFFSETTING BOOK IS WORST UNDER LOW CORRELATION. This is the whole
			// reason FRTB is a max over three scenarios rather than one number: a
			// bank may not assume the netting that happens to help it. Reversing
			// the comparison is the failure that hands back the most flattering
			// scenario instead of the worst.
			Name: "an_offsetting_book_is_worst_under_low_correlation_definitional",
			Got: boolAsFloat(
				frtb.ChargeForScenario(offsetting, frtbParams(0.5, 0.5), frtb.Low) >
					frtb.ChargeForScenario(offsetting, frtbParams(0.5, 0.5), frtb.High)),
			Want: 1, Tolerance: 0.5,
		},
		{
			// AND AN ALIGNED BOOK IS WORST UNDER HIGH CORRELATION — the opposite
			// direction on the same inputs. A charge that ignored the scenario
			// entirely passes the case above and fails this one.
			Name: "an_aligned_book_is_worst_under_high_correlation_definitional",
			Got: boolAsFloat(
				frtb.ChargeForScenario(aligned, frtbParams(0.5, 0.5), frtb.High) >
					frtb.ChargeForScenario(aligned, frtbParams(0.5, 0.5), frtb.Low)),
			Want: 1, Tolerance: 0.5,
		},
		{
			// THE AGGREGATE IS THE MAX OF THE THREE, exactly — not the sum, not the
			// average, not the medium one. Graded as a number rather than as an
			// inequality so an implementation returning the sum cannot satisfy it.
			Name: "the_charge_is_the_worst_of_the_three_scenarios_definitional",
			Got:  frtb.Charge(offsetting, frtb.Params{frtbClass: frtbParams(0.5, 0.5)}),
			Want: max(
				frtb.ChargeForScenario(offsetting, frtbParams(0.5, 0.5), frtb.Low),
				frtb.ChargeForScenario(offsetting, frtbParams(0.5, 0.5), frtb.Medium),
				frtb.ChargeForScenario(offsetting, frtbParams(0.5, 0.5), frtb.High)),
			Tolerance: 1e-12,
		},
	}
}

// ===== MAR21.4(4) and MAR21.4(5): across buckets =====

func frtbCrossBucketCases() []validation.Case {
	// Two buckets, each with two ALIGNED factors, the buckets OFFSETTING each
	// other. At ρ=0 every Kb is √50 = 7.0711 while |Sb| = 10, so Kb < |Sb| and the
	// cross term can outrun Σ Kb².
	opposed := []frtb.Sensitivity{
		frtbSens("1", "X", 5), frtbSens("1", "Y", 5),
		frtbSens("2", "X", -5), frtbSens("2", "Y", -5),
	}
	// γ = 0.8 > ρ = 0 makes the radicand −20 in the low scenario, −60 in medium
	// and −100 in high. MAR21.4(5)'s alternative Sb = max[min(ΣWS, Kb), −Kb] gives
	// ±√50, so the low-scenario radicand becomes 100 − 0.6·100 = 40 and the charge
	// √40 = 6.324555. The worst of the three is the low scenario.
	opposedParams := frtbParams(0, 0.8)
	altLow := math.Sqrt(2*50 - 0.6*2*50)

	// Two buckets that do NOT offset — the ordinary case, radicand positive.
	sameWay := []frtb.Sensitivity{frtbSens("1", "X", 30), frtbSens("2", "Y", 40)}

	return []validation.Case{
		{
			// THE CASE THAT FOUND SOMETHING. Before the MAR21.4(5) repair this read
			// 0.000000: a book of four sensitivities of magnitude 5 required no
			// capital at all, because the engine substituted zero for a negative
			// radicand instead of recomputing with Sb capped to ±Kb.
			Name: "two_offsetting_buckets_are_not_free_definitional",
			Got:  frtb.Charge(opposed, frtb.Params{frtbClass: opposedParams}),
			Want: altLow, Tolerance: 1e-9,
		},
		{
			// AND THE SAME BOOK IS NOT ZERO IN ANY INDIVIDUAL SCENARIO EITHER,
			// except the high one where γ reaches 1 and the alternative radicand is
			// genuinely zero. Stated as "the medium scenario is strictly positive"
			// so the case above cannot be satisfied by a single lucky branch.
			Name: "the_alternative_specification_applies_in_every_scenario_definitional",
			Got: boolAsFloat(
				frtb.ChargeForScenario(opposed, opposedParams, frtb.Medium) > 0 &&
					frtb.ChargeForScenario(opposed, opposedParams, frtb.Low) > 0),
			Want: 1, Tolerance: 0.5,
		},
		{
			// THE ALTERNATIVE IS CONDITIONAL, and this is the case that says so.
			// The same offsetting book at γ=0.4 has radicand 100 − 0.4·200 = 20,
			// which is POSITIVE, so Sb stays uncapped and the charge is √20.
			// Applying the cap unconditionally — which is what curvature.go does,
			// correctly, under MAR21.5(4) — would return √60 here and shrink every
			// ordinary offsetting book. Copying the repair to the wrong scope is a
			// live risk precisely because the right version is next door.
			Name: "the_alternative_specification_is_not_applied_to_a_positive_radicand_definitional",
			Got:  frtb.ChargeForScenario(opposed, frtbParams(0, 0.4), frtb.Medium),
			Want: math.Sqrt(20), Tolerance: 1e-9,
		},
		{
			// THE CAPPED RADICAND, HAND-COMPUTED ON AN ASYMMETRIC BOOK. Buckets of
			// (+5,+5) and (−4,−8) at ρ=0 give K = √50 and √80 against |S| = 10 and
			// 12, so the caps bite by different amounts and a single shared cap
			// cannot reproduce the answer. γ=0.8 ⇒ raw radicand 130 − 192 = −62,
			// capped radicand 130 − 1.6·√50·√80.
			//
			// It is the case that fails if the alternative substitutes Sb = 0 rather
			// than Sb = ±Kb — which returns √130, larger and still plausible.
			Name: "the_alternative_specification_matches_the_hand_computed_capped_root_definitional",
			Got: frtb.ChargeForScenario([]frtb.Sensitivity{
				frtbSens("1", "X", 5), frtbSens("1", "Y", 5),
				frtbSens("2", "X", -4), frtbSens("2", "Y", -8),
			}, frtbParams(0, 0.8), frtb.Medium),
			Want:      math.Sqrt(50 + 80 - 0.8*2*math.Sqrt(50)*math.Sqrt(80)),
			Tolerance: 1e-9,
		},
		{
			// THE ORDINARY CROSS-BUCKET SUM, hand-computed, so the repair above did
			// not change the path that was already right:
			// γ=0.5 ⇒ √(900 + 1600 + 0.5·2·30·40) = √3700.
			Name: "the_cross_bucket_aggregation_matches_the_hand_computed_root_definitional",
			Got:  frtb.ChargeForScenario(sameWay, frtbParams(0.5, 0.5), frtb.Medium),
			Want: math.Sqrt(3700), Tolerance: 1e-9,
		},
		{
			// γ AND ρ ARE DIFFERENT KNOBS. Same four sensitivities split across two
			// buckets rather than one, with ρ and γ deliberately unequal: if the
			// two were transposed the answer would be √(2·50 + 0.9·2·50) instead.
			Name: "the_inter_bucket_correlation_is_not_the_intra_bucket_one_definitional",
			Got: frtb.ChargeForScenario([]frtb.Sensitivity{
				frtbSens("1", "X", 5), frtbSens("1", "Y", 5),
				frtbSens("2", "X", 5), frtbSens("2", "Y", 5),
			}, frtbParams(0.9, 0.1), frtb.Medium),
			Want: math.Sqrt(2*(50+0.9*2*25) + 0.1*2*10*10), Tolerance: 1e-9,
		},
	}
}

// ===== structural properties of the charge as a whole =====

func frtbStructuralCases() []validation.Case {
	book := []frtb.Sensitivity{
		frtbSens("1", "X", 30), frtbSens("1", "Y", -40), frtbSens("2", "Z", 25),
	}
	p := frtb.Params{frtbClass: frtbParams(0.5, 0.5)}
	base := frtb.Charge(book, p)

	doubled := make([]frtb.Sensitivity, len(book))
	for i, s := range book {
		doubled[i] = frtb.Sensitivity{RiskClass: s.RiskClass, Bucket: s.Bucket, Factor: s.Factor, Amount: 2 * s.Amount}
	}

	// A second risk class, so cross-class additivity is graded on a book that has
	// two of them.
	twoClass := append(append([]frtb.Sensitivity{}, book...),
		frtb.Sensitivity{RiskClass: "OTHER", Bucket: "1", Factor: "W", Amount: 80})
	twoClassParams := frtb.Params{
		frtbClass: frtbParams(0.5, 0.5),
		"OTHER":   frtbParams(0.3, 0.3),
	}

	return []validation.Case{
		{
			// HOMOGENEOUS OF DEGREE ONE. Every term under the root is quadratic in
			// the sensitivities, so doubling the book doubles the capital exactly.
			// A charge that were not would make the filing depend on the unit the
			// sensitivities were expressed in.
			Name: "the_charge_is_homogeneous_of_degree_one_in_the_sensitivities_definitional",
			Got:  frtb.Charge(doubled, p),
			Want: 2 * base, Tolerance: 1e-9,
		},
		{
			// AND LINEAR IN THE RISK WEIGHT, which is the same statement made about
			// the other multiplicand. It is the property that lets a deployment
			// swap the representative calibration for the published one and reason
			// about the effect.
			Name: "the_charge_is_linear_in_the_risk_weight_definitional",
			Got: frtb.Charge(book, frtb.Params{frtbClass: frtb.ClassParams{
				RiskWeight: map[string]float64{"1": 3, "2": 3},
				IntraCorr:  0.5, InterCorr: 0.5,
			}}),
			Want: 3 * base, Tolerance: 1e-9,
		},
		{
			// CAPITAL SUMS ACROSS RISK CLASSES WITH NO DIVERSIFICATION BENEFIT. The
			// SBM correlates within a class and adds across them; a cross-class
			// correlation invented here would understate every multi-asset book.
			Name: "capital_sums_across_risk_classes_definitional",
			Got:  frtb.Charge(twoClass, twoClassParams),
			Want: frtb.Charge(book, twoClassParams) +
				frtb.Charge([]frtb.Sensitivity{twoClass[len(twoClass)-1]}, twoClassParams),
			Tolerance: 1e-9,
		},
		{
			// NO SENSITIVITY IS NO CAPITAL. The baseline every other number here is
			// measured against, and the one an implementation with an additive
			// constant would fail.
			Name: "an_empty_book_carries_no_capital_definitional",
			Got:  frtb.Charge(nil, p),
			Want: 0, Tolerance: 1e-12,
		},
		{
			// A NON-EMPTY BOOK IS NOT ZERO. The mirror of the case above, and the
			// one that stops "return 0" from passing the whole set. It is also the
			// shape the MAR21.4(5) defect took, so it is stated as a standalone
			// property rather than left implicit in the numbers.
			Name: "a_book_with_gross_risk_carries_capital_definitional",
			Got:  boolAsFloat(base > 0),
			Want: 1, Tolerance: 0.5,
		},
		{
			// THE CHARGE IS AT LEAST THE LARGEST SINGLE NET FACTOR. Correlations
			// live in [0,1], so no amount of netting can take the capital below the
			// biggest weighted sensitivity standing alone — 40 here. It is a bound
			// rather than a value, which makes it survive a change of calibration.
			Name: "the_charge_is_at_least_the_largest_net_weighted_sensitivity_definitional",
			Got:  boolAsFloat(base >= 40),
			Want: 1, Tolerance: 0.5,
		},
	}
}
