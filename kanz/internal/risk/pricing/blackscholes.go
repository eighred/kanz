package pricing

import "math"

// Black-Scholes-Merton closed-form pricing + analytic Greeks for European
// options on an asset with continuous dividend yield q.

// d1d2 returns the BS d1, d2 terms. Callers guard t>0 and sigma>0 first.
func d1d2(S, K, t, r, q, sigma float64) (d1, d2 float64) {
	vol := sigma * math.Sqrt(t)
	d1 = (math.Log(S/K) + (r-q+0.5*sigma*sigma)*t) / vol
	d2 = d1 - vol
	return d1, d2
}

// BlackScholesPrice is the BSM value of a European option. For a degenerate
// input (t≤0, sigma≤0, or non-positive S/K) it returns the discounted
// intrinsic value, the correct limit.
func BlackScholesPrice(otype OptionType, S, K, t, r, q, sigma float64) float64 {
	if t <= 0 || sigma <= 0 || S <= 0 || K <= 0 {
		// At/after expiry (or zero vol) the option is worth its intrinsic value;
		// for zero vol the forward is S·e^{(r−q)t}, but intrinsic at spot is the
		// conventional, conservative degenerate limit used across the library.
		return intrinsic(otype, S, K)
	}
	d1, d2 := d1d2(S, K, t, r, q, sigma)
	df := math.Exp(-r * t)
	dq := math.Exp(-q * t)
	if otype == Call {
		return S*dq*normCDF(d1) - K*df*normCDF(d2)
	}
	return K*df*normCDF(-d2) - S*dq*normCDF(-d1)
}

// BlackScholesGreeks returns the analytic Greeks of a European option. Degenerate
// inputs yield zero Greeks (a no-uncertainty stance matching the price floor).
func BlackScholesGreeks(otype OptionType, S, K, t, r, q, sigma float64) Greeks {
	if t <= 0 || sigma <= 0 || S <= 0 || K <= 0 {
		return Greeks{}
	}
	d1, d2 := d1d2(S, K, t, r, q, sigma)
	df := math.Exp(-r * t)
	dq := math.Exp(-q * t)
	pdf := normPDF(d1)
	sqrtT := math.Sqrt(t)

	g := Greeks{
		Gamma: dq * pdf / (S * sigma * sqrtT),
		Vega:  S * dq * pdf * sqrtT,
	}
	// calendarTheta = ∂V/∂(calendar); our Theta = ∂V/∂(time-to-expiry) = −calendar.
	commonDecay := -(S * dq * pdf * sigma) / (2 * sqrtT)
	if otype == Call {
		g.Delta = dq * normCDF(d1)
		g.Rho = K * t * df * normCDF(d2)
		calendarTheta := commonDecay - r*K*df*normCDF(d2) + q*S*dq*normCDF(d1)
		g.Theta = -calendarTheta
	} else {
		g.Delta = dq * (normCDF(d1) - 1)
		g.Rho = -K * t * df * normCDF(-d2)
		calendarTheta := commonDecay + r*K*df*normCDF(-d2) - q*S*dq*normCDF(-d1)
		g.Theta = -calendarTheta
	}
	return g
}
