package benchmarks

import (
	"math"

	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/validation"
)

// THE BUMPED (FINITE-DIFFERENCE) GREEKS ENGINE (#471).
//
// This is the lattice's model-agnostic sensitivity engine: it reprices under a
// shifted input and divides. It is what produces the Greeks for AMERICAN
// options, where no closed form exists — so it is the one Greek path with
// nothing to check it against by construction, and the reason it needs a
// benchmark rather than an assumption.
//
// # Two kinds of evidence, and the second is the sharper one
//
// CROSS-MODEL comparisons against the closed form are the obvious test and the
// weaker one: a bumped derivative on a discrete lattice carries the lattice's
// own noise, so the tolerance has to be loose enough to admit it, and a loose
// tolerance admits real errors of the same size. Gamma is the worst case — it is
// a second derivative, so the 1/h² in the difference quotient amplifies the
// node-spacing quantisation.
//
// THE IDENTITIES ARE NEARLY EXACT, and that is not luck. Put-call delta parity,
// the equality of a call's and a put's gamma, and an American call on a
// non-dividend payer matching the European one are all properties of the SAME
// lattice, so the discretisation error is common to both sides and cancels.
// Measured rather than hoped: the call/put gamma difference is 2.5e-14 and the
// American/European Greeks are bit-identical at EVERY step count tried, against a
// cross-model gamma error around 5e-3. An identity is therefore some twelve
// orders of magnitude more sensitive here than the comparison to the closed form,
// and it stays that way when the lattice gets cheaper.
//
// # Where the tolerances come from, and why not from one step count
//
// CRR CONVERGENCE IS NOT MONOTONE. Measured at Hull's parameters, the deviation
// of bumped vega from the closed form runs
//
//	n=250  9.5e-2      n=400  8.8e-3      n=500  9.8e-2      n=800  8.2e-2
//
// — an eleven-fold swing between two adjacent step counts, because the strike
// falls differently between lattice nodes at each. Reading the error at ONE n and
// setting the tolerance just above it would fit the benchmark to that lattice's
// luck, and a harmless change to the step count would then fail the build.
//
// So the cross-model tolerances are set above the OSCILLATION ENVELOPE across
// that range rather than at the chosen n. They are loose, deliberately, and the
// identities below are what carry the precision.
//
// # Why 250 steps
//
// A STARTUP-COST DECISION, not an accuracy one, and worth stating because it
// looks like the latter. LoadValidations runs at the risk-engine composition
// root BEFORE readiness, so every benchmark here is on the boot path. At 2000
// steps this set alone took 2.1s and pushed Reports() to 4.6s; at 250 it is
// 36ms. The identities are unaffected — they are exact at every n, because both
// sides share one lattice and the discretisation error cancels — and the
// cross-model cases were never going to be precise enough for the extra 2
// seconds to buy anything.

// AnalyticGreeksFiniteDiff is the bumped-Greeks engine.
const AnalyticGreeksFiniteDiff = "greeks_finite_diff"

// The same option Hull's worked example uses, so the closed-form side of every
// cross-model case below is already anchored to a published value.
const greekSteps = 250

// GreeksFiniteDifference grades the bumped engine against the closed form it
// approximates, and against the identities both must satisfy.
func GreeksFiniteDifference() []validation.Case {
	call := pricing.BinomialGreeks(pricing.Call, pricing.European,
		hullS, hullK, hullT, hullR, hullQ, hullV, greekSteps)
	put := pricing.BinomialGreeks(pricing.Put, pricing.European,
		hullS, hullK, hullT, hullR, hullQ, hullV, greekSteps)
	american := pricing.BinomialGreeks(pricing.Call, pricing.American,
		hullS, hullK, hullT, hullR, hullQ, hullV, greekSteps)
	closed := pricing.BlackScholesGreeks(pricing.Call,
		hullS, hullK, hullT, hullR, hullQ, hullV)

	return []validation.Case{
		// ===== CROSS-MODEL: the bump must approximate the derivative =====
		//
		// The closed-form side is Hull-anchored (see BlackScholes above), so
		// these inherit a published reference at one remove.
		{
			Name: "bumped_delta_converges_to_the_closed_form_cross_model",
			Got:  call.Delta, Want: closed.Delta, Tolerance: 3e-3,
		},
		{
			// The loosest tolerance here, and the reason is the method: gamma is
			// a SECOND derivative, so the 1/h² in the quotient amplifies node
			// quantisation. 1.5e-2 on a gamma of 0.05 is 30% — which sounds
			// useless until you note the identity below pins the SAME quantity to
			// 1e-9. The cross-model case rules out an error of the wrong order of
			// magnitude; the identity rules out everything else.
			Name: "bumped_gamma_converges_to_the_closed_form_cross_model",
			Got:  call.Gamma, Want: closed.Gamma, Tolerance: 1.5e-2,
		},
		{
			Name: "bumped_vega_converges_to_the_closed_form_cross_model",
			Got:  call.Vega, Want: closed.Vega, Tolerance: 1.5e-1,
		},
		{
			Name: "bumped_theta_converges_to_the_closed_form_cross_model",
			Got:  call.Theta, Want: closed.Theta, Tolerance: 5e-2,
		},
		{
			Name: "bumped_rho_converges_to_the_closed_form_cross_model",
			Got:  call.Rho, Want: closed.Rho, Tolerance: 2e-2,
		},

		// ===== IDENTITIES: same lattice on both sides, so the error cancels ====
		{
			// PUT-CALL PARITY IN DELTA: Δcall − Δput = e^{−qT}, which is 1 at
			// q = 0. Differentiating the parity relation gives it exactly, and it
			// catches a sign or discounting error in the bump that leaves each
			// delta individually plausible.
			Name: "put_call_delta_parity_identity",
			Got:  call.Delta - put.Delta, Want: math.Exp(-hullQ * hullT), Tolerance: 1e-9,
		},
		{
			// A CALL AND A PUT ON THE SAME STRIKE HAVE THE SAME GAMMA. Their
			// prices differ by a linear function of spot, whose second derivative
			// is zero. Nearly exact on one lattice (measured 2.5e-14 at 250 steps,
			// and the same order at every count tried), which makes this some
			// twelve orders of magnitude sharper than the cross-model gamma case
			// above — on the very same quantity.
			Name: "call_and_put_share_one_gamma_identity",
			Got:  call.Gamma - put.Gamma, Want: 0, Tolerance: 1e-9,
		},
		{
			// AND THE SAME VEGA, for the same reason: the parity difference does
			// not depend on volatility.
			Name: "call_and_put_share_one_vega_identity",
			Got:  call.Vega - put.Vega, Want: 0, Tolerance: 1e-6,
		},

		// ===== THE AMERICAN PATH, which has no closed form at all =====
		//
		// An American call on a non-dividend-paying underlying is never exercised
		// early, so every Greek must equal the European one. This is the ONLY
		// check available on the early-exercise branch — the branch that exists
		// solely for American options and that no European comparison reaches.
		{
			Name: "american_call_no_dividend_matches_european_delta_identity",
			Got:  american.Delta, Want: call.Delta, Tolerance: 1e-12,
		},
		{
			Name: "american_call_no_dividend_matches_european_gamma_identity",
			Got:  american.Gamma, Want: call.Gamma, Tolerance: 1e-12,
		},
		{
			Name: "american_call_no_dividend_matches_european_vega_identity",
			Got:  american.Vega, Want: call.Vega, Tolerance: 1e-12,
		},

		// ===== PUBLISHED BOUNDS =====
		{
			// A CALL'S DELTA LIES IN (0,1). Published, and it fails on the whole
			// class of bump errors that scale the derivative — a delta of 78 or
			// 0.0078 is still a number, and only the bound says which.
			Name: "call_delta_is_within_its_published_bounds",
			Got:  boolAsFloat(call.Delta > 0 && call.Delta < 1), Want: 1, Tolerance: 0.5,
		},
		{
			// A PUT'S DELTA LIES IN (−1,0). The mirror bound, so a sign error that
			// happens to keep the call inside its range is still caught.
			Name: "put_delta_is_within_its_published_bounds",
			Got:  boolAsFloat(put.Delta > -1 && put.Delta < 0), Want: 1, Tolerance: 0.5,
		},
		{
			// GAMMA AND VEGA ARE POSITIVE for a long option of either type. A
			// negative one is not a small error; it inverts every hedge computed
			// from it.
			Name: "gamma_and_vega_are_positive_for_a_long_option_published",
			Got: boolAsFloat(call.Gamma > 0 && put.Gamma > 0 &&
				call.Vega > 0 && put.Vega > 0), Want: 1, Tolerance: 0.5,
		},
	}
}
