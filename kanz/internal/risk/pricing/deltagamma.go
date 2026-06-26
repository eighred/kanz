package pricing

import "math"

// Delta-gamma P&L approximation and a parametric VaR variant for option books
// (DERIV-01e). Full revaluation reprices each option exactly under a shock; the
// delta-gamma approximation is the cheap second-order alternative the desk uses
// for fast what-ifs and parametric VaR:
//
//	ΔV ≈ Δ·dS + ½·Γ·dS²
//
// where Δ, Γ are the DOLLAR Greeks (∂V/∂S × position, ∂²V/∂S² × position) and dS
// is the underlying move. The gamma term is what a delta-only VaR misses — it is
// the convexity that makes a long-option book lose less (and a short-option book
// lose more) than a linear estimate on a large move.

// DeltaGammaPnL returns the second-order P&L estimate for a dollar move dS in the
// underlying, given dollar delta and dollar gamma.
func DeltaGammaPnL(dollarDelta, dollarGamma, dS float64) float64 {
	return dollarDelta*dS + 0.5*dollarGamma*dS*dS
}

// DeltaGammaVaR returns the one-tailed parametric Value-at-Risk (a positive loss
// number) of a position/book under a normal return shock, using the delta-gamma
// expansion. spot is the underlying level, sigmaReturn its one-sigma periodic
// return volatility, and z the confidence z-score (e.g. 2.326 for 99%). The move
// taken is the adverse z·σ return in the direction that loses money:
//
//	dS = ±z·σ·S , VaR = −min(PnL(+dS), PnL(−dS))
//
// Evaluating both signs captures the asymmetry the gamma term introduces, so a
// short-gamma book's VaR reflects its true downside rather than a symmetric
// delta-only figure.
func DeltaGammaVaR(dollarDelta, dollarGamma, spot, sigmaReturn, z float64) float64 {
	if spot <= 0 || sigmaReturn <= 0 || z <= 0 {
		return 0
	}
	move := z * sigmaReturn * spot
	pnlUp := DeltaGammaPnL(dollarDelta, dollarGamma, move)
	pnlDown := DeltaGammaPnL(dollarDelta, dollarGamma, -move)
	worst := math.Min(pnlUp, pnlDown)
	if worst >= 0 {
		return 0 // both directions profit (long gamma, delta-neutral) ⇒ no VaR loss
	}
	return -worst
}
