package structured

import (
	"fmt"
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
//
// IT REFUSES A TRANCHE IT CANNOT DISCOUNT (#572). Both refusal arms used to
// return 0: a RateEnv with no curve — the state a StructuredSpec assembled
// without one arrives in — and an index the projection does not hold. A price of
// zero for a performing senior tranche then flows straight into
// EffectiveRisk, whose p0==0 guard turns it into an effective duration and
// convexity of exactly zero. Nothing in that chain was an error, and the number
// at the end is a claim.
func PriceTranche(proj Projection, idx int, env RateEnv, spread float64) (float64, error) {
	t, err := proj.Tranche(idx)
	if err != nil {
		return 0, err
	}
	if env.Curve == nil {
		return 0, fmt.Errorf("%w: the rate environment carries no discount curve, so tranche %q "+
			"has no present value", ErrUnpriceable, t.Name)
	}
	var pv float64
	for m := range t.Principal {
		yr := float64(m+1) / 12
		cf := t.Interest[m] + t.Principal[m]
		df := env.Curve.Discount(yr) * math.Exp(-spread*yr)
		pv += cf * df
	}
	return pv, nil
}

// OAS solves for the constant spread over the curve that reprices tranche idx to
// price. The projection is option-adjusted: the cashflows already reflect the
// rate-dependent prepayment (the curve drives the behavioral model), so the
// residual spread is the option-adjusted spread. Bisection on the monotone
// (decreasing in spread) price function.
func OAS(d Deal, pm PrepayModel, env RateEnv, idx int, price float64) (float64, error) {
	proj := d.Project(pm, env)
	// PROBED ONCE, BEFORE THE SOLVE. bisect calls f two hundred times and cannot
	// carry an error out of any of them; a refusal inside the objective would
	// otherwise be swallowed and the solver would converge on the spread that
	// makes zero equal the price — a number with no relationship to the deal.
	if _, err := PriceTranche(proj, idx, env, 0); err != nil {
		return 0, err
	}
	f := func(s float64) float64 {
		pv, err := PriceTranche(proj, idx, env, s)
		if err != nil {
			return 0
		}
		return pv - price
	}
	return bisect(f, -0.5, 0.5, 1e-10), nil
}

// EffectiveRisk returns the effective duration and effective convexity of tranche
// idx at the given OAS, by repricing under ±bp parallel curve shifts with the
// prepay model re-run on each shifted curve:
//
//	effDur  = (P₋ − P₊) / (2·P₀·Δy)
//	effConv = (P₊ + P₋ − 2·P₀) / (P₀·Δy²)
//
// THE p0==0 ARM WAS THE WHOLE DEFECT (#572). It read `if p0 == 0 { return 0, 0 }`
// — a division-by-zero guard that answered with the number the division was
// supposed to produce. Every way of reaching it is a fault (no curve, an index
// the deal does not have, a tranche written down to nothing), and every one of
// them came back as "this position has no rate risk", weighted into the book's
// duration average by its full market value. That is #565's zero-capital risk
// class, in a measure rather than a filing.
//
// A zero bump size is refused for the same reason: dy==0 makes both formulas
// 0/0, and the IEEE answer is a NaN that decimal conversion renders as something
// worse than a refusal.
func EffectiveRisk(d Deal, pm PrepayModel, env RateEnv, idx int, oas, bp float64) (effDur, effConv float64, err error) {
	if bp == 0 {
		return 0, 0, fmt.Errorf("%w: an effective duration needs a non-zero bump size", ErrUnpriceable)
	}
	p0, err := PriceTranche(d.Project(pm, env), idx, env, oas)
	if err != nil {
		return 0, 0, err
	}
	if p0 == 0 {
		return 0, 0, fmt.Errorf("%w: tranche %d prices to zero at an OAS of %g, so its relative "+
			"rate sensitivity is 0/0", ErrUnpriceable, idx, oas)
	}
	up := shiftEnv(env, bp)
	down := shiftEnv(env, -bp)
	pUp, err := PriceTranche(d.Project(pm, up), idx, up, oas)
	if err != nil {
		return 0, 0, err
	}
	pDown, err := PriceTranche(d.Project(pm, down), idx, down, oas)
	if err != nil {
		return 0, 0, err
	}
	dy := bp / 10000 // basis points → decimal
	effDur = (pDown - pUp) / (2 * p0 * dy)
	effConv = (pUp + pDown - 2*p0) / (p0 * dy * dy)
	return effDur, effConv, nil
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
