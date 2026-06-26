// Package pricing is the risk engine's derivative pricing library (DERIV-01b).
// It replaces the RISK-07 placeholder Delta with real per-instrument option
// pricing and full Greeks: closed-form Black-Scholes for European options, a
// Cox-Ross-Rubinstein binomial tree for American ones, and Greeks both
// analytically (Black-Scholes) and by bumping (binomial / model-agnostic).
//
// # Float internals, Decimal at the edges
//
// Pricing is irreducibly transcendental (exp, ln, normal CDF), so the math runs
// in float64 — the same stance compute.volatility / the VaR layer take. Inputs
// arrive and results leave as common.v1.Decimal at the measure boundary
// (greeks.go); within this package everything is float64. The "double is banned"
// rule is about money/size values of record, not derived analytics.
//
// # Conventions
//
// Time t is in years to expiry. r is the continuously-compounded risk-free rate;
// q the continuous dividend/borrow yield (0 by default). Greeks are the partial
// derivatives with respect to their natural variable:
//
//   - Delta = ∂V/∂S, Gamma = ∂²V/∂S², Vega = ∂V/∂σ (per 1.00 of vol),
//     Rho = ∂V/∂r (per 1.00 of rate), Theta = ∂V/∂t (w.r.t. time-TO-EXPIRY;
//     the calendar time-decay a desk quotes is its negation, −Theta).
//
// Keeping every Greek a derivative w.r.t. its own variable means each validates
// against a central finite difference of the price in that variable (DERIV-01f),
// with no sign bookkeeping.
package pricing

import "math"

// OptionType is the right an option confers — mirrors reference.v1.OptionType
// without importing the proto into the hot pricing path.
type OptionType int

const (
	// Call is the right to buy the underlying at the strike.
	Call OptionType = iota
	// Put is the right to sell the underlying at the strike.
	Put
)

// Exercise is when an option may be exercised.
type Exercise int

const (
	// European: exercisable only at expiry (closed-form Black-Scholes).
	European Exercise = iota
	// American: exercisable any time up to expiry (binomial).
	American
)

// Greeks are the first/second-order sensitivities of an option value. See the
// package doc for the derivative convention.
type Greeks struct {
	Delta float64 // ∂V/∂S
	Gamma float64 // ∂²V/∂S²
	Vega  float64 // ∂V/∂σ (per 1.00 of vol)
	Theta float64 // ∂V/∂t (time-to-expiry; calendar decay is −Theta)
	Rho   float64 // ∂V/∂r (per 1.00 of rate)
}

// DiscountCurve supplies the continuously-compounded zero rate for a maturity t
// (years). It is the seam the FI-01 yield-curve construction plugs into; until
// FI-01 lands a FlatCurve is the default the engine wires. Keeping pricers
// scalar-r (not curve-aware) keeps them pure; the curve is resolved at the
// per-position layer (greeks.go).
type DiscountCurve interface {
	// Rate returns the continuously-compounded zero rate for maturity t years.
	Rate(t float64) float64
}

// FlatCurve is a constant-rate DiscountCurve.
type FlatCurve float64

// Rate implements DiscountCurve.
func (c FlatCurve) Rate(float64) float64 { return float64(c) }

var _ DiscountCurve = FlatCurve(0)

// Price returns the option value under the appropriate model: closed-form
// Black-Scholes for European, the binomial tree (DefaultBinomialSteps) for
// American. q is the continuous dividend yield.
func Price(otype OptionType, exercise Exercise, S, K, t, r, q, sigma float64) float64 {
	if exercise == American {
		return BinomialPrice(otype, exercise, S, K, t, r, q, sigma, DefaultBinomialSteps)
	}
	return BlackScholesPrice(otype, S, K, t, r, q, sigma)
}

// PriceGreeks returns price + Greeks: analytic for European (Black-Scholes),
// bumped for American (binomial). One call so a caller prices and risks an
// option together.
func PriceGreeks(otype OptionType, exercise Exercise, S, K, t, r, q, sigma float64) (float64, Greeks) {
	if exercise == American {
		return BinomialPrice(otype, exercise, S, K, t, r, q, sigma, DefaultBinomialSteps),
			BinomialGreeks(otype, exercise, S, K, t, r, q, sigma, DefaultBinomialSteps)
	}
	return BlackScholesPrice(otype, S, K, t, r, q, sigma), BlackScholesGreeks(otype, S, K, t, r, q, sigma)
}

// intrinsic is the exercise value at spot S — the price floor and the t≤0 value.
func intrinsic(otype OptionType, S, K float64) float64 {
	if otype == Call {
		return math.Max(S-K, 0)
	}
	return math.Max(K-S, 0)
}

// normCDF is the standard-normal cumulative distribution Φ(x), via erfc for
// numerical stability in the tails.
func normCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }

// normPDF is the standard-normal density φ(x).
func normPDF(x float64) float64 { return math.Exp(-0.5*x*x) / math.Sqrt(2*math.Pi) }
