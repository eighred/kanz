package xva

import "github.com/eighred/kanz/internal/risk/pricing"

// Concrete Pricer implementations for the exposure simulation. These reprice a
// trade as the simulated risk factors evolve — the DERIV-01 / FI-01 pricers
// plugged into the XVA-01b Pricer seam. A production deployment wires real
// reference terms + curves here at the composition root; these cover the linear
// and option cases the tests and the common derivatives book need.

// LinearTrade is a forward/linear claim on a single risk factor: notional units
// of the factor struck at Strike, valued (factor − Strike)·Notional. A long
// forward, an equity TRS leg, or an FX forward. The simplest exposure generator
// — its value is monotone in the factor, so its exposure grows with the factor's
// diffusion (the PFE-monotone-in-vol/horizon property).
type LinearTrade struct {
	FactorID string
	Strike   float64
	Notional float64 // signed: positive long, negative short
}

// Value implements Pricer (time-independent — a linear claim has no decay).
func (t LinearTrade) Value(_ float64, state MarketState) float64 {
	return (state[t.FactorID] - t.Strike) * t.Notional
}

// OptionTrade is a European option on a single risk factor (the underlying spot),
// repriced with the DERIV-01b Black-Scholes pricer as the spot evolves. The
// time-to-expiry passed to the pricer is the residual maturity at the simulation
// date; the simulator supplies the spot through the state, the rest is static
// contract data.
type OptionTrade struct {
	FactorID string
	Type     pricing.OptionType
	Strike   float64
	Expiry   float64 // years from t=0
	Rate     float64
	Dividend float64
	Vol      float64
	Quantity float64 // signed contracts × multiplier
}

// Value implements Pricer — Black-Scholes value at the residual maturity
// (Expiry − valuation time t). A trade at/after expiry is worth its intrinsic
// value (the pricer handles ttm≤0).
func (o OptionTrade) Value(t float64, state MarketState) float64 {
	ttm := o.Expiry - t
	if ttm < 0 {
		ttm = 0
	}
	px := pricing.BlackScholesPrice(o.Type, state[o.FactorID], o.Strike, ttm, o.Rate, o.Dividend, o.Vol)
	return px * o.Quantity
}
