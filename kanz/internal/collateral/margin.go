// Package collateral is the COLL-01 collateral / margin / financing plane — the
// cash-and-collateral machinery that turns positions into FUNDED positions
// (ROI #33): variation + initial (ISDA SIMM) margin, cheapest-to-deliver
// collateral optimization, and repo / securities-lending financing with a
// forward cash ladder. It is the downstream of the XVA-01 CSA — the same terms
// that reduce counterparty exposure are what a margin call moves.
//
// # Boundary (RISK-02): SIMM sensitivities are an INPUT, not a reach-in
//
// collateral lives OUTSIDE kanz/internal/risk, so the arch test forbids it from
// importing the risk impl packages (the DERIV-01 Greek pricers, compute, domain).
// That is exactly how ISDA SIMM is meant to work: it is a SENSITIVITY-based
// margin (the CRIF input), so the engine takes the delta/vega sensitivities as
// data — a deployment computes Greeks via the risk engine and feeds them in —
// the same "covariance/Greeks are an input" stance OPT-01 (its own
// SampleCovariance) and performance (its own Classifier) take.
//
// # Money convention
//
// The engine works in float64 internally (margins are derived risk statistics,
// like VaR), with common.v1.Decimal at the wire edge (collateral.v1) — the
// established compute stance (float internals, Decimal at the boundary).
package collateral

import "math"

// CSATerms are the margin-relevant economics of a collateral agreement — the
// float working shape behind collateral.v1.CollateralAgreement.
type CSATerms struct {
	// Threshold is the unsecured amount below which no collateral is posted.
	Threshold float64
	// MinTransferAmount is the smallest call that is actually made.
	MinTransferAmount float64
	// IndependentAmount is always-posted over-collateralization.
	IndependentAmount float64
	// Rounding is the increment a call is rounded to (0 ⇒ no rounding). Calls in
	// our favor round up, calls we owe round down — the CSA convention that never
	// rounds against the collateral holder.
	Rounding float64
}

// VariationMargin computes the variation-margin call for one agreement given the
// current netted mark-to-market (positive = the counterparty owes us) and the
// collateral already held. The required collateral is the exposure above the
// threshold plus the independent amount; the call is the gap to it, suppressed
// below the MTA and rounded. A positive result is collateral the counterparty
// posts to us; negative is collateral we return/post.
//
// VM tracks MtM: as the mark rises, the required collateral and the call rise
// one-for-one above the threshold (the COLL-01e property).
func VariationMargin(mtm, heldCollateral float64, t CSATerms) (call, required float64) {
	required = math.Max(mtm-t.Threshold, 0) + t.IndependentAmount
	call = required - heldCollateral
	if math.Abs(call) < t.MinTransferAmount {
		return 0, required
	}
	return roundCall(call, t.Rounding), required
}

// roundCall rounds a call to the agreement increment, never against the holder:
// a call in our favor (positive) rounds up, a call we owe (negative) rounds down
// (toward a larger absolute posting to us / smaller return).
func roundCall(call, increment float64) float64 {
	if increment <= 0 {
		return call
	}
	if call >= 0 {
		return math.Ceil(call/increment) * increment
	}
	return math.Floor(call/increment) * increment
}
