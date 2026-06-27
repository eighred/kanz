package xva

import "math"

// XVA-01c — the valuation adjustments derived from an exposure profile, a credit
// curve, and a funding spread. Each is a discounted integral over the exposure
// profile weighted by the relevant probability:
//
//	CVA = LGD_cpty · Σ_k EE(t_k)·DF(t_k)·[Q_cpty(t_{k-1}) − Q_cpty(t_k)]
//	DVA = LGD_own  · Σ_k NEE(t_k)·DF(t_k)·[Q_own(t_{k-1})  − Q_own(t_k)]
//	FVA ≈ s_fund   · Σ_k EE(t_k)·DF(t_k)·Q_cpty(t_k)·Δt_k
//
// CVA is the price of the counterparty's default risk (a cost, reported
// positive); DVA the symmetric benefit from our own default; FVA the cost of
// funding the uncollateralized exposure over its life.

// Adjustments bundles the inputs the valuation adjustments integrate.
type Adjustments struct {
	// Profile is the netting set's exposure profile (XVA-01b).
	Profile ExposureProfile
	// Counterparty is the counterparty's credit curve (drives CVA/FVA survival).
	Counterparty CreditCurve
	// Own is our own credit curve (drives DVA). Zero value ⇒ DVA is 0.
	Own CreditCurve
	// DiscountRate is the flat continuously-compounded discount rate.
	DiscountRate float64
	// FundingSpread is our funding spread over the risk-free rate (decimal).
	FundingSpread float64
}

// CVA is the credit valuation adjustment — the discounted expected loss from the
// counterparty defaulting while we are in-the-money. Reported as a positive cost.
func (a Adjustments) CVA() float64 {
	return creditIntegral(a.Profile.Times, a.Profile.EE, a.Counterparty, a.DiscountRate)
}

// DVA is the debit valuation adjustment — the symmetric benefit from our own
// default while we are out-of-the-money. Reported positive (a benefit that
// offsets CVA in the bilateral price).
func (a Adjustments) DVA() float64 {
	return creditIntegral(a.Profile.Times, a.Profile.NEE, a.Own, a.DiscountRate)
}

// FVA is the funding valuation adjustment — the discounted funding cost of
// carrying the uncollateralized expected exposure over its life, while the
// counterparty survives.
func (a Adjustments) FVA() float64 {
	times, ee := a.Profile.Times, a.Profile.EE
	var fva, prevT float64
	for k, t := range times {
		dt := t - prevT
		prevT = t
		df := math.Exp(-a.DiscountRate * t)
		fva += a.FundingSpread * ee[k] * df * a.Counterparty.Survival(t) * dt
	}
	return fva
}

// BCVA is the bilateral CVA, CVA − DVA — the net counterparty-risk price.
func (a Adjustments) BCVA() float64 { return a.CVA() - a.DVA() }

// creditIntegral is the shared CVA/DVA quadrature: Σ_k exposure_k·DF(t_k)·
// marginal-default-prob(t_{k-1}, t_k), scaled by LGD. A zero-hazard curve
// (no tenors) contributes nothing.
func creditIntegral(times, exposure []float64, curve CreditCurve, rate float64) float64 {
	if len(curve.Hazards) == 0 {
		return 0
	}
	lgd := curve.LGD()
	var sum, prevT float64
	for k, t := range times {
		pd := curve.DefaultProbability(prevT, t)
		df := math.Exp(-rate * t)
		sum += exposure[k] * df * pd
		prevT = t
	}
	return lgd * sum
}
