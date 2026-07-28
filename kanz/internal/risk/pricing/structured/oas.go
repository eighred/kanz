package structured

import (
	"math"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
)

// STRUCT-01d — OAS and rate risk. PriceTranche discounts a tranche's projected
// cashflows on the FI-01 curve plus a constant spread; OAS is the spread that
// reprices the tranche to a market price; effective duration/convexity reprice
// under parallel curve shifts WITH the prepayment model re-run on the shifted
// curve — so the negative convexity of a callable mortgage (it shortens as rates
// fall) falls out of the measurement, not an analytic assumption.

// PriceTranche is the present value of a tranche's (interest + principal)
// cashflows discounted on the env curve plus a continuously-compounded spread.
func PriceTranche(proj Projection, idx int, env RateEnv, spread float64) float64 {
	if env.Curve == nil || idx < 0 || idx >= len(proj.Tranches) {
		return 0
	}
	t := proj.Tranches[idx]
	var pv float64
	for m := range t.Principal {
		yr := float64(m+1) / 12
		cf := t.Interest[m] + t.Principal[m]
		df := env.Curve.Discount(yr) * math.Exp(-spread*yr)
		pv += cf * df
	}
	return pv
}

// OAS solves for the constant spread over the curve that reprices tranche idx to
// price. The projection is option-adjusted: the cashflows already reflect the
// rate-dependent prepayment (the curve drives the behavioral model), so the
// residual spread is the option-adjusted spread. Bisection on the monotone
// (decreasing in spread) price function.
func OAS(d Deal, pm PrepayModel, env RateEnv, idx int, price float64) float64 {
	proj := d.Project(pm, env)
	f := func(s float64) float64 { return PriceTranche(proj, idx, env, s) - price }
	return bisect(f, -0.5, 0.5, 1e-10)
}

// EffectiveRisk returns the effective duration and effective convexity of tranche
// idx at the given OAS, by repricing under ±bp parallel curve shifts with the
// prepay model re-run on each shifted curve:
//
//	effDur  = (P₋ − P₊) / (2·P₀·Δy)
//	effConv = (P₊ + P₋ − 2·P₀) / (P₀·Δy²)
func EffectiveRisk(d Deal, pm PrepayModel, env RateEnv, idx int, oas, bp float64) (effDur, effConv float64) {
	p0 := PriceTranche(d.Project(pm, env), idx, env, oas)
	if p0 == 0 {
		return 0, 0
	}
	up := shiftEnv(env, bp)
	down := shiftEnv(env, -bp)
	pUp := PriceTranche(d.Project(pm, up), idx, up, oas)
	pDown := PriceTranche(d.Project(pm, down), idx, down, oas)
	dy := bp / 10000 // basis points → decimal
	effDur = (pDown - pUp) / (2 * p0 * dy)
	effConv = (pUp + pDown - 2*p0) / (p0 * dy * dy)
	return effDur, effConv
}

// shiftEnv applies a parallel bp shift to the env's curve (reusing the FI-01
// curve.Parallel primitive), keeping the reference tenor.
func shiftEnv(env RateEnv, bp float64) RateEnv {
	if env.Curve == nil {
		return env
	}
	return RateEnv{Curve: curve.Parallel{Bp: bp}.Apply(env.Curve), RefTenor: env.RefTenor}
}

// bisect finds a root of f on [lo, hi] (f(lo) and f(hi) must straddle 0), to an
// absolute tolerance tol on f. Falls back to the closer endpoint if it cannot
// bracket — a price outside the spread range pins to the nearest spread.
func bisect(f func(float64) float64, lo, hi, tol float64) float64 {
	flo, fhi := f(lo), f(hi)
	if flo == 0 {
		return lo
	}
	if fhi == 0 {
		return hi
	}
	if flo*fhi > 0 {
		if math.Abs(flo) < math.Abs(fhi) {
			return lo
		}
		return hi
	}
	for i := 0; i < 200; i++ {
		mid := 0.5 * (lo + hi)
		fm := f(mid)
		if math.Abs(fm) < tol || (hi-lo) < 1e-12 {
			return mid
		}
		if flo*fm < 0 {
			hi = mid
		} else {
			lo, flo = mid, fm
		}
	}
	return 0.5 * (lo + hi)
}
