package curve

import (
	"errors"
	"math"
	"sort"
)

// Bootstrapping a zero curve from par instrument quotes (FI-01b).
//
// A par quote is a coupon-paying instrument (par swap or par bond) trading at
// par: its fixed rate equals its yield, so its price is exactly 1 (per unit
// notional). The bootstrap solves, pillar by pillar in ascending tenor, for the
// discount factor that makes each successive par instrument price to par given
// the discount factors of the shorter ones already solved:
//
//	1 = c_n · Σ_{i<n} τ_i·DF_i  +  (1 + c_n·τ_n)·DF_n
//	⇒ DF_n = (1 − c_n·Σ_{i<n} τ_i·DF_i) / (1 + c_n·τ_n)
//
// where c_n is the par rate at pillar n and τ_i is the accrual fraction of
// period i. Each par instrument's coupon dates are taken to be the pillar tenors
// up to its own (τ_i = tenor_i − tenor_{i−1}, τ_1 = tenor_1). This is the
// textbook sequential bootstrap; repricing any input par instrument off the
// resulting curve recovers par exactly (the FI-01f round-trip property).

// ParQuote is one par-instrument input to the bootstrap: a tenor (years) and the
// par rate (e.g. 0.045 for a 4.5% par swap).
type ParQuote struct {
	Tenor float64
	Rate  float64
}

// ErrParBootstrap is returned when the par quotes cannot be bootstrapped (no
// quotes, non-ascending tenors, or a non-positive solved discount factor — an
// arbitrageable / malformed quote set).
var ErrParBootstrap = errors.New("curve: cannot bootstrap par quotes")

// Bootstrap builds a zero curve from par quotes. Quotes need not be pre-sorted.
// The resulting curve stores continuously-compounded zeros and interpolates per
// interp. A solved discount factor that is non-positive (or a non-ascending /
// non-positive tenor) yields ErrParBootstrap rather than a garbage curve.
func Bootstrap(quotes []ParQuote, interp Interpolation) (*Curve, error) {
	if len(quotes) == 0 || !validInterpolation(interp) {
		return nil, ErrParBootstrap
	}
	for _, q := range quotes {
		if !finite(q.Tenor, q.Rate) {
			return nil, ErrParBootstrap
		}
	}
	qs := make([]ParQuote, len(quotes))
	copy(qs, quotes)
	sort.Slice(qs, func(i, j int) bool { return qs[i].Tenor < qs[j].Tenor })

	tenors := make([]float64, len(qs))
	zeros := make([]float64, len(qs))
	prevTenor := 0.0
	annuity := 0.0
	for n, q := range qs {
		if q.Tenor <= 0 || q.Tenor <= prevTenor && n > 0 {
			return nil, ErrParBootstrap
		}
		// Every input uses the same prefix of coupon dates. Retain its annuity
		// rather than summing that identical prefix again for each pillar.
		tauN := q.Tenor - prevTenor
		numerator := 1 - q.Rate*annuity
		denominator := 1 + q.Rate*tauN
		dfN := numerator / denominator
		// A positive factor above one is valid under negative rates. Positivity
		// and finite arithmetic are required; descending discounts are not policy.
		if !finite(annuity, numerator, denominator, dfN) || numerator <= 0 || denominator <= 0 || dfN <= 0 {
			return nil, ErrParBootstrap
		}
		// Taking log of a rounded DF=1 erases sub-epsilon nonzero rates.
		lnDF := math.Log1p(-q.Rate*annuity) - math.Log1p(q.Rate*tauN)
		zeros[n] = -lnDF / q.Tenor
		if !finite(zeros[n]) {
			return nil, ErrParBootstrap
		}
		tenors[n] = q.Tenor
		annuity += tauN * dfN
		if !finite(annuity) {
			return nil, ErrParBootstrap
		}
		prevTenor = q.Tenor
	}
	return NewZeroCurve(tenors, zeros, Continuous, interp)
}

// ParRate reprices a par instrument of the given tenor off the curve: the fixed
// rate c that makes 1 = c·Σ τ_i·DF_i + DF_tenor, i.e. the curve's implied par
// yield at that tenor. couponTimes are the coupon dates (years, ascending,
// ending at tenor); an absent/invalid schedule is unavailable (NaN). The
// inverse of Bootstrap — repricing a bootstrap input recovers its quote.
func (c *Curve) ParRate(couponTimes []float64) float64 {
	if len(couponTimes) == 0 {
		return math.NaN()
	}
	annuity := 0.0
	prev := 0.0
	for _, t := range couponTimes {
		if !finite(t) || t <= prev {
			return math.NaN()
		}
		annuity += (t - prev) * c.Discount(t)
		prev = t
	}
	last := couponTimes[len(couponTimes)-1]
	if !finite(annuity) || annuity <= 0 {
		return math.NaN()
	}
	return finiteOrNaN(-math.Expm1(-c.Zero(last)*last) / annuity)
}
