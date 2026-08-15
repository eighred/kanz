// Package benchmarks is the independent evidence the SR 11-7 validation gate
// grades (#471) — model validation, not Go performance benchmarks.
//
// # Why this package exists separately from internal/validation
//
// The gate takes benchmark results as DATA and imports no analytic code, which
// is what keeps it unable to break a pricing path. Something still has to RUN
// the analytic and compare it to an independently published value, and that is
// this package.
//
// IT LIVES INSIDE internal/risk/ RATHER THAN BESIDE THE GATE, because running an
// analytic means importing the impl packages, and the RISK-02 boundary reserves
// those for the risk module itself — everyone outside it goes through
// internal/risk/api/v*. Sitting under internal/validation would have made this
// the first outsider to reach past that contract for "just one helper", which is
// exactly the regression test/arch/risk_boundary_test.go exists to stop. The
// direction is the right one anyway: the evidence belongs with the analytics it
// grades, and the gate stays ignorant of both.
//
// # What "independent" means here, stated per case rather than claimed once
//
// A validation is only worth the independence of its benchmark. Three kinds
// appear below and they are NOT equally strong, so each case says which it is:
//
//   - PUBLISHED — the expected value comes from a source outside this codebase
//     (Hull's worked examples). The strongest evidence: it was computed by
//     someone who had never seen this implementation.
//   - IDENTITY — an arbitrage relation the output must satisfy whatever the
//     implementation (put-call parity). It cannot confirm the level is right,
//     only that the two sides are consistent — but it fails loudly on a whole
//     class of sign and discounting errors.
//   - CROSS-MODEL — two independent implementations that must agree (the CRR
//     tree against the closed form). It catches an error in either, and proves
//     neither if both are wrong the same way.
//
// THE EXPECTED VALUES ARE NEVER READ OUT OF THE CODE. A benchmark whose Want was
// produced by the analytic it grades validates nothing — it is a change-detector
// wearing a control's name, and it passes forever including on the day the
// analytic is wrong. Every PUBLISHED value below is transcribed from the cited
// source; the implementation's agreement with them is the finding, not the input.
//
// # This is a floor, not a ceiling
//
// #471's own suggested slice: record benchmarks for the analytics that HAVE
// published independent benchmarks, then export how much of the library that
// covers — including the zeroes. VaR/ES, SIMM, FRTB and the SVI fit are NOT here
// yet, and the posture metric is what makes that visible rather than something a
// reader has to infer from this file's length.
package benchmarks

import (
	"math"
	"time"

	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/validation"
)

// Analytic names, used as the gate's key and as the metric label. They are the
// identity a validation report is filed under, so they are constants rather than
// strings at the call site.
const (
	AnalyticBlackScholes = "black_scholes"
	AnalyticBinomial     = "binomial_crr"
)

// Hull's canonical European option, from Options, Futures and Other Derivatives
// — the worked example used to introduce the Black-Scholes formula.
//
//	S = 42, K = 40, r = 10%, sigma = 20%, T = 0.5 years, no dividend
//	call = 4.76, put = 0.81, d1 = 0.7693, d2 = 0.6278
const (
	hullS, hullK, hullT = 42.0, 40.0, 0.5
	hullR, hullQ, hullV = 0.10, 0.0, 0.20

	hullCall = 4.76 // PUBLISHED
	hullPut  = 0.81 // PUBLISHED
	// hullDelta is N(d1) for d1 = 0.7693, the value Hull carries through the
	// same example when he introduces delta.
	hullDelta = 0.7791 // PUBLISHED

	// hullPriceTol is half a cent either side of a figure Hull quotes to the
	// cent. Tightening it below the source's own precision would fail on the
	// rounding in the book rather than on anything about this implementation.
	hullPriceTol = 0.005
	// hullDeltaTol matches the four decimals the cited value carries.
	hullDeltaTol = 0.0001
)

// BlackScholes grades the closed-form pricer against Hull's worked example and
// against put-call parity.
func BlackScholes() []validation.Case {
	call := pricing.BlackScholesPrice(pricing.Call, hullS, hullK, hullT, hullR, hullQ, hullV)
	put := pricing.BlackScholesPrice(pricing.Put, hullS, hullK, hullT, hullR, hullQ, hullV)
	greeks := pricing.BlackScholesGreeks(pricing.Call, hullS, hullK, hullT, hullR, hullQ, hullV)

	// PUT-CALL PARITY: C - P = S·e^(-qT) - K·e^(-rT). An identity, so the
	// expected value is arithmetic on the INPUTS and never on an output — which
	// is what keeps this case independent of the pricer it grades.
	parity := hullS*math.Exp(-hullQ*hullT) - hullK*math.Exp(-hullR*hullT)

	return []validation.Case{
		{
			Name: "hull_european_call_published", // PUBLISHED
			Got:  call, Want: hullCall, Tolerance: hullPriceTol,
		},
		{
			Name: "hull_european_put_published", // PUBLISHED
			Got:  put, Want: hullPut, Tolerance: hullPriceTol,
		},
		{
			Name: "hull_call_delta_published", // PUBLISHED
			Got:  greeks.Delta, Want: hullDelta, Tolerance: hullDeltaTol,
		},
		{
			// IDENTITY. Catches a sign error on either leg and a discounting
			// error on the strike — a class of mistake that leaves both prices
			// individually plausible.
			Name: "put_call_parity_identity",
			Got:  call - put, Want: parity, Tolerance: 1e-9,
		},
	}
}

// Binomial grades the CRR tree against the closed form it must converge to.
//
// CROSS-MODEL, and weaker than a published value — it proves the two agree, not
// that either is right. It is here because the tree is the AMERICAN pricer and
// has no closed form to be published against; agreement on the European case,
// where both are defined, is the strongest independent statement available about
// the lattice, the risk-neutral probability and the discounting.
//
// It also grades the tree against Hull's published European call directly, which
// is a PUBLISHED case and does not depend on this codebase's own Black-Scholes
// being right.
func Binomial() []validation.Case {
	const steps = 2000
	tree := pricing.BinomialPrice(pricing.Call, pricing.European,
		hullS, hullK, hullT, hullR, hullQ, hullV, steps)
	closed := pricing.BlackScholesPrice(pricing.Call, hullS, hullK, hullT, hullR, hullQ, hullV)

	return []validation.Case{
		{
			Name: "hull_european_call_published", // PUBLISHED
			Got:  tree, Want: hullCall, Tolerance: hullPriceTol,
		},
		{
			// CROSS-MODEL. A CRR tree converges as O(1/steps); at 2000 steps a
			// cent is comfortable and still tight enough to fail on a wrong
			// up/down factor or risk-neutral probability, which move the answer
			// by far more than the discretisation does.
			Name: "crr_converges_to_closed_form",
			Got:  tree, Want: closed, Tolerance: 0.01,
		},
		{
			// AN AMERICAN CALL ON A NON-DIVIDEND-PAYING UNDERLYING IS NEVER
			// EXERCISED EARLY, so it must price exactly as the European one.
			// IDENTITY, and it is the one case here that exercises the early
			// exercise branch — the branch that exists only for American options
			// and that a European-only comparison never reaches.
			Name: "american_call_no_dividend_equals_european",
			Got: pricing.BinomialPrice(pricing.Call, pricing.American,
				hullS, hullK, hullT, hullR, hullQ, hullV, steps),
			Want: tree, Tolerance: 1e-9,
		},
	}
}

// Reports validates every analytic this package carries evidence for, at now.
//
// IT RETURNS FAILING REPORTS TOO. A failed validation is audit evidence — it is
// signed and filed exactly like a passing one, and the gate simply never
// promotes on it. Dropping it here would make "the analytic failed its benchmark"
// and "nobody has benchmarked this analytic" the same observable state, which is
// the distinction #471 exists to draw.
func Reports(now time.Time, signer validation.Signer) ([]validation.Report, error) {
	sets := []struct {
		analytic string
		cases    []validation.Case
	}{
		{AnalyticBlackScholes, BlackScholes()},
		{AnalyticBinomial, Binomial()},
		{AnalyticVaRBacktest, VaRBacktest()},
		{AnalyticBondAnalytics, BondAnalytics()},
	}
	out := make([]validation.Report, 0, len(sets))
	for _, s := range sets {
		r, err := validation.Validate(s.analytic, s.cases, now, 0, signer)
		if err != nil {
			// A malformed case set is a defect in THIS package, not a failed
			// validation, and must not be filed as evidence of either outcome.
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Inventory is every analytic this platform expects to be validated, whether or
// not evidence exists for it yet.
//
// THE ZEROES ARE THE POINT. A metric that reported only the analytics with
// evidence would climb from nothing to nothing and always read as full coverage.
// #471's question is "how much of this book is priced by something nobody
// checked", and that is only answerable if the unchecked ones are named.
//
// HAND-MAINTAINED, AND THAT IS THE KNOWN WEAKNESS. Nothing in Go marks a
// function as "an analytic", so an analytic added without a line here is
// invisible to the count — the coverage would look better than it is, which is
// the wrong direction for a control. It is a list rather than a discovery
// because the alternatives are worse: a package walk would count helpers and
// constructors, and a naming convention would be a second contract to keep.
// Reviewing this list is part of adding an analytic.
//
// The names are drawn from #471's own enumeration of what serves production
// today.
func Inventory() []string {
	return []string{
		AnalyticBlackScholes,
		AnalyticBinomial,
		AnalyticVaRBacktest,
		AnalyticBondAnalytics,
		// value_at_risk IS NOT var_backtest, and listing both is the point.
		// Validating the exception test says nothing about whether the
		// historical-simulation quantile is right; collapsing them would let one
		// benchmark set mark two analytics green.
		"value_at_risk",      // internal/risk/compute/var
		"expected_shortfall", // internal/risk/compute/var
		"greeks_finite_diff", // internal/risk/compute/greeks.go
		"svi_vol_surface",    // internal/risk/pricing/volsurface
		"cds_bootstrap",      // internal/risk/pricing/credit
		"isda_simm",          // internal/collateral
		"frtb_sa",            // internal/regulatory
		"stress_framework",   // internal/regulatory/stress
	}
}
