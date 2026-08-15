package benchmarks

import (
	"math"

	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/risk/pricing/volsurface"
	"github.com/eighred/kanz/internal/validation"
)

// THE SVI VOLATILITY SURFACE AND ITS ARBITRAGE GATE (#471).
//
// # What this validates, precisely
//
// The raw-SVI slice — w(k) = a + b(ρ(k−m) + √((k−m)² + σ²)) — its published
// closed forms, and Gatheral's butterfly-density gate that decides whether a
// fitted surface may be served at all. Plus ImpliedVol, because FitSVI backs out
// every quote through it before fitting anything: a surface fitted to wrong
// implied vols is wrong in a way no surface-level property can see.
//
// # Why the gate matters more than the fit
//
// A mis-fitted smile prices an option a little wrong. A surface admitting
// BUTTERFLY ARBITRAGE prices one at a NEGATIVE IMPLIED DENSITY — the model
// asserts a butterfly spread worth less than nothing — and every Greek taken off
// it inherits that. The package's own doc says a violating quote set "errors
// rather than feeding the Greek pricers a surface admitting negative densities",
// so the gate is the load-bearing part and it is where the published test vector
// lives.
//
// # THE PUBLISHED COUNTEREXAMPLE
//
// Axel Vogt's parameters, (a,b,ρ,m,σ) = (−0.0410, 0.1331, 0.3060, 0.3586,
// 0.4153), are the standard butterfly-arbitrage example in Gatheral & Jacquier,
// "Arbitrage-free SVI volatility surfaces" (2014). They look entirely ordinary —
// σ and b are plausible, the minimum variance a + bσ√(1−ρ²) = 0.0116 is
// positive — and g(k) reaches −0.0329 near k = 0.879.
//
// That is the sharpest case available: an arbitrage-free-looking slice that a
// correct gate must REFUSE. A gate that accepted everything would satisfy every
// other case in this file.
//
// # And the case that states the gate's limit
//
// ButterflyFree takes an evaluation range, and Vogt's violation sits at k ≈
// 0.879. Asked about [−0.5, 0.5] the same parameters come back ARBITRAGE-FREE —
// correctly, because there is no arbitrage in that range. It is asserted here so
// the limitation is a recorded property rather than a surprise: the gate is only
// as wide as the moneyness range a caller hands it, and a caller that checks a
// narrow window around the money has not checked the wings where SVI arbitrage
// actually lives.

// AnalyticSVIVolSurface is the SVI surface and its arbitrage gate.
const AnalyticSVIVolSurface = "svi_vol_surface"

// A well-behaved slice: 20% ATM vol at one year (w(0) = 0.06 total variance),
// modest skew. Every closed form below is evaluated on it.
var benignSVI = volsurface.SVIParams{A: 0.04, B: 0.10, Rho: -0.30, M: 0.00, Sigma: 0.20}

// vogtSVI is the published butterfly-arbitrage counterexample. See the doc above.
var vogtSVI = volsurface.SVIParams{A: -0.0410, B: 0.1331, Rho: 0.3060, M: 0.3586, Sigma: 0.4153}

// SVIVolSurface grades the surface against its published forms and its gate.
func SVIVolSurface() []validation.Case {
	p := benignSVI

	// The published closed forms of the raw parameterization.
	kMin := p.M - p.Rho*p.Sigma/math.Sqrt(1-p.Rho*p.Rho)
	wMin := p.A + p.B*p.Sigma*math.Sqrt(1-p.Rho*p.Rho)

	cases := []validation.Case{
		// ===== THE PARAMETERIZATION ITSELF =====
		{
			// w(m) = a + bσ exactly: at k = m the square root is σ and the ρ term
			// vanishes. The one point of the formula that can be checked without
			// evaluating a square root, so it isolates the ρ·(k−m) term.
			Name: "raw_svi_at_the_vertex_is_a_plus_b_sigma_definitional",
			Got:  p.TotalVar(p.M), Want: p.A + p.B*p.Sigma, Tolerance: 1e-12,
		},
		{
			Name: "raw_svi_matches_the_published_formula_at_positive_k_definitional",
			Got:  p.TotalVar(0.5), Want: 0.078851648071, Tolerance: 1e-9,
		},
		{
			// THE ASYMMETRY IS THE SKEW. With ρ < 0 the left wing is higher, and a
			// sign error in the ρ term produces a surface that is smooth, positive,
			// plausible, and skewed the wrong way — which prices every put wrong.
			Name: "raw_svi_matches_the_published_formula_at_negative_k_definitional",
			Got:  p.TotalVar(-0.5), Want: 0.108851648071, Tolerance: 1e-9,
		},
		{
			Name: "the_left_wing_is_higher_than_the_right_for_negative_rho_published",
			Got:  boolAsFloat(p.TotalVar(-0.5) > p.TotalVar(0.5)), Want: 1, Tolerance: 0.5,
		},

		// ===== THE MINIMUM, in closed form =====
		{
			// dw/dk = 0 at k = m − ρσ/√(1−ρ²), where w = a + bσ√(1−ρ²). Published,
			// and it is the quantity the no-arbitrage condition "minimum variance
			// non-negative" is stated over — so getting it wrong makes that check
			// test a different number than the literature's.
			Name: "total_variance_at_the_closed_form_minimum_matches_published",
			Got:  p.TotalVar(kMin), Want: wMin, Tolerance: 1e-12,
		},
		{
			// AND IT REALLY IS THE MINIMUM, over a wide grid. The closed form above
			// could be right while the function it is claimed to minimise is not
			// the one being evaluated.
			Name: "no_point_on_the_slice_lies_below_the_closed_form_minimum_published",
			Got:  boolAsFloat(sviMinOnGrid(p, -3, 3) >= wMin-1e-12), Want: 1, Tolerance: 0.5,
		},
		{
			// TOTAL VARIANCE IS STRICTLY POSITIVE. A slice touching zero is a zero
			// implied vol, which prices every option at intrinsic.
			Name: "total_variance_is_positive_across_the_quoted_range_published",
			Got:  boolAsFloat(sviMinOnGrid(p, -3, 3) > 0), Want: 1, Tolerance: 0.5,
		},

		// ===== THE WING ASYMPTOTES =====
		{
			// w(k)/k → b(1+ρ) as k → +∞ (Gatheral). Approached rather than reached,
			// so the tolerance admits the 1/k tail: measured 2.0e-3 at k=20 and
			// 2.0e-4 at k=200.
			Name: "right_wing_slope_approaches_b_times_one_plus_rho_published",
			Got:  p.TotalVar(200) / 200, Want: p.B * (1 + p.Rho), Tolerance: 1e-3,
		},
		{
			Name: "left_wing_slope_approaches_b_times_one_minus_rho_published",
			Got:  p.TotalVar(-200) / 200, Want: p.B * (1 - p.Rho), Tolerance: 1e-3,
		},
		{
			// AND THE GAP SHRINKS WITH |k| — the convergence, which is the sharper
			// statement. A wrong asymptote would sit at a constant offset and pass a
			// loose point tolerance.
			Name: "the_wing_slope_converges_rather_than_merely_being_close_published",
			Got: boolAsFloat(math.Abs(p.TotalVar(200)/200-p.B*(1+p.Rho)) <
				math.Abs(p.TotalVar(20)/20-p.B*(1+p.Rho))), Want: 1, Tolerance: 0.5,
		},
		{
			// LEE'S MOMENT FORMULA bounds the wing slopes at 2; a slope above it
			// implies a distribution with no finite moment of the corresponding
			// order. Published, and it is the check that a fitted b has not run away.
			Name: "both_wing_slopes_respect_lees_bound_of_two_published",
			Got:  boolAsFloat(p.B*(1+p.Rho) <= 2 && p.B*(1-p.Rho) <= 2),
			Want: 1, Tolerance: 0.5,
		},

		// ===== THE ARBITRAGE GATE =====
		{
			// NON-VACUITY for the case below: a well-behaved slice must PASS, or a
			// gate that refuses everything would satisfy the counterexample.
			Name: "a_well_behaved_slice_is_accepted_by_the_butterfly_gate",
			Got:  boolAsFloat(p.ButterflyFree(-1.5, 1.5)), Want: 1, Tolerance: 0.5,
		},
		{
			// THE PUBLISHED COUNTEREXAMPLE. Gatheral & Jacquier (2014) give Vogt's
			// parameters as a slice that admits butterfly arbitrage; g(k) reaches
			// −0.0329 near k = 0.879. A gate that accepts this serves a surface with
			// a negative implied density.
			Name: "vogts_published_parameters_are_refused_by_the_butterfly_gate_published",
			Got:  boolAsFloat(!vogtSVI.ButterflyFree(-1.5, 1.5)), Want: 1, Tolerance: 0.5,
		},
		{
			// THE GATE IS ONLY AS WIDE AS THE RANGE IT IS GIVEN. Vogt's violation
			// sits outside [−0.5, 0.5], so over that window the same parameters are
			// correctly arbitrage-free. Recorded so the limitation is a property
			// rather than a surprise: checking a narrow window around the money does
			// not check the wings, which is where SVI arbitrage lives.
			Name: "the_butterfly_gate_only_covers_the_range_it_is_asked_about_definitional",
			Got:  boolAsFloat(vogtSVI.ButterflyFree(-0.5, 0.5)), Want: 1, Tolerance: 0.5,
		},
		{
			// VOGT'S SLICE LOOKS ORDINARY on every other measure — its minimum
			// variance is positive. That is what makes it a good test vector, and
			// what makes eyeballing a surface an inadequate substitute for the gate.
			Name: "vogts_minimum_variance_is_positive_so_the_gate_is_not_catching_that_published",
			Got:  boolAsFloat(vogtSVI.A+vogtSVI.B*vogtSVI.Sigma*math.Sqrt(1-vogtSVI.Rho*vogtSVI.Rho) > 0),
			Want: 1, Tolerance: 0.5,
		},
	}

	// ===== IMPLIED VOL: the input every fit is built on =====
	//
	// A ROUND TRIP, and it is definitional: price a call at a known vol with the
	// same Black-Scholes the pricer uses, hand the price back, and the vol must
	// come out. It fails on any error in the solver's bracketing, its convergence
	// criterion, or the forward/discounting convention — and every one of those
	// produces a surface fitted to numbers that are not the market's vols.
	for _, v := range []struct {
		vol   float64
		price float64
	}{
		{0.15, 6.9618416446},
		{0.25, 10.8705584906},
		{0.40, 16.7044171996},
	} {
		got, ok := volsurface.ImpliedVol(pricing.Call, v.price, 100, 100, 1, 0.02, 0)
		cases = append(cases, validation.Case{
			Name: "implied_vol_round_trips_a_black_scholes_price_definitional_" + trimFloat(v.vol*100),
			Got:  boolFloat(ok, got), Want: v.vol, Tolerance: 1e-6,
		})
	}
	cases = append(cases, validation.Case{
		// A PRICE BELOW INTRINSIC HAS NO IMPLIED VOL. Returning one would put a
		// fabricated number into the smile the surface is fitted to — and a bad
		// quote is exactly what a fitted surface must not smooth over.
		Name: "a_price_below_intrinsic_yields_no_implied_vol_definitional",
		Got: boolAsFloat(func() bool {
			_, ok := volsurface.ImpliedVol(pricing.Call, 0.01, 100, 50, 1, 0.02, 0)
			return !ok
		}()), Want: 1, Tolerance: 0.5,
	})

	return cases
}

// sviMinOnGrid returns the smallest total variance on [lo,hi].
func sviMinOnGrid(p volsurface.SVIParams, lo, hi float64) float64 {
	min := math.Inf(1)
	const steps = 2000
	for i := 0; i <= steps; i++ {
		k := lo + (hi-lo)*float64(i)/steps
		if w := p.TotalVar(k); w < min {
			min = w
		}
	}
	return min
}

// boolFloat returns got when ok, and a value that cannot pass any tolerance when
// not — so "the solver refused" fails the case loudly instead of comparing a zero
// against a small expected vol and looking merely inaccurate.
func boolFloat(ok bool, got float64) float64 {
	if !ok {
		return math.NaN()
	}
	return got
}
