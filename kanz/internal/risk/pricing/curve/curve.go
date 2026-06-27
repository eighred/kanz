// Package curve is the FI-01b yield-curve construction layer: it bootstraps a
// zero/discount curve from par/swap quotes and answers discount factors and zero
// rates at any tenor by interpolation. It is the real implementation behind the
// DERIV-01b pricing.DiscountCurve seam — until FI-01 a FlatCurve was the only
// curve the option pricers could discount off; a *Curve now plugs in.
//
// # Float internals, point-in-time at the edge
//
// Like the pricing package, curve math runs in float64 (it is exp/ln-heavy and
// feeds the float pricers). A curve is built point-in-time from a reference
// snapshot (the YieldCurve schema, FI-01a) resolved at an as-of via the store;
// this package is the pure constructor + evaluator and holds no I/O.
//
// # Conventions
//
// Tenors are in years. Internally every curve is stored as continuously-
// compounded zero rates on ascending pillars, so DF(t) = exp(-z(t)·t) — the same
// continuously-compounded r the Black-Scholes/binomial pricers expect from
// DiscountCurve.Rate. Inputs given under another compounding convention are
// converted to continuous on construction.
package curve

import (
	"errors"
	"math"
	"sort"
)

// Interpolation selects how the curve is interpolated between pillar tenors.
type Interpolation int

const (
	// LinearZero interpolates linearly in the continuously-compounded zero rate.
	LinearZero Interpolation = iota
	// LogLinearDF interpolates linearly in ln(discount factor) — equivalently
	// piecewise-constant instantaneous forward rates, the no-arbitrage default.
	LogLinearDF
)

// Compounding is the convention an input zero rate compounds under. The curve
// stores continuous internally; NewZeroCurve converts.
type Compounding int

const (
	// Continuous: DF = exp(-r·t).
	Continuous Compounding = iota
	// Annual: DF = (1+r)^-t.
	Annual
	// SemiAnnual: DF = (1+r/2)^-2t.
	SemiAnnual
)

// Curve is a term structure of continuously-compounded zero rates on ascending
// pillar tenors. The zero value is not usable — construct via NewZeroCurve or
// Bootstrap.
type Curve struct {
	tenors []float64 // ascending pillar tenors (years)
	zeros  []float64 // continuously-compounded zero rate at each pillar
	interp Interpolation
}

var (
	// ErrNoPillars is returned when a curve is built with no pillars.
	ErrNoPillars = errors.New("curve: at least one pillar required")
	// ErrNonAscending is returned when pillar tenors are not strictly ascending
	// (positive).
	ErrNonAscending = errors.New("curve: pillar tenors must be strictly ascending and positive")
)

// NewZeroCurve builds a curve directly from (tenor, zero-rate) pillars under the
// given compounding. Pillars need not be pre-sorted; they are sorted and checked
// for strict ascendancy. Rates are converted to the continuous internal basis.
func NewZeroCurve(tenors, rates []float64, comp Compounding, interp Interpolation) (*Curve, error) {
	if len(tenors) == 0 || len(tenors) != len(rates) {
		return nil, ErrNoPillars
	}
	type pillar struct{ t, r float64 }
	ps := make([]pillar, len(tenors))
	for i := range tenors {
		ps[i] = pillar{tenors[i], toContinuous(rates[i], comp, tenors[i])}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].t < ps[j].t })
	c := &Curve{
		tenors: make([]float64, len(ps)),
		zeros:  make([]float64, len(ps)),
		interp: interp,
	}
	for i, p := range ps {
		if p.t <= 0 || (i > 0 && p.t <= c.tenors[i-1]) {
			return nil, ErrNonAscending
		}
		c.tenors[i] = p.t
		c.zeros[i] = p.r
	}
	return c, nil
}

// toContinuous converts a zero rate quoted under comp at tenor t to its
// continuously-compounded equivalent: the rate c with exp(-c·t) == the input
// discount factor.
func toContinuous(r float64, comp Compounding, t float64) float64 {
	switch comp {
	case Annual:
		return math.Log(1 + r)
	case SemiAnnual:
		return 2 * math.Log(1+r/2)
	default:
		return r
	}
}

// Zero returns the continuously-compounded zero rate at tenor t (years) by
// interpolation. Flat extrapolation beyond the pillars (never invents curvature
// past the data — the volsurface clamp stance).
func (c *Curve) Zero(t float64) float64 {
	n := len(c.tenors)
	if t <= c.tenors[0] {
		return c.zeros[0]
	}
	if t >= c.tenors[n-1] {
		return c.zeros[n-1]
	}
	i := sort.SearchFloat64s(c.tenors, t) // first index with tenor >= t
	t0, t1 := c.tenors[i-1], c.tenors[i]
	z0, z1 := c.zeros[i-1], c.zeros[i]
	w := (t - t0) / (t1 - t0)
	if c.interp == LogLinearDF {
		// Linear in ln(DF) = -z·t between the two pillars.
		l0, l1 := -z0*t0, -z1*t1
		lnDF := l0 + w*(l1-l0)
		return -lnDF / t
	}
	return z0 + w*(z1-z0) // LinearZero
}

// Discount returns the discount factor DF(t) = exp(-z(t)·t). t<=0 ⇒ 1.
func (c *Curve) Discount(t float64) float64 {
	if t <= 0 {
		return 1
	}
	return math.Exp(-c.Zero(t) * t)
}

// Rate implements pricing.DiscountCurve: the continuously-compounded zero rate
// for maturity t years. This is the seam DERIV-01b's FlatCurve held open.
func (c *Curve) Rate(t float64) float64 {
	if t <= 0 {
		return c.zeros[0]
	}
	return c.Zero(t)
}

// Forward returns the continuously-compounded forward rate between t1 and t2
// (t1 < t2): f = (z2·t2 − z1·t1) / (t2 − t1).
func (c *Curve) Forward(t1, t2 float64) float64 {
	if t2 <= t1 {
		return c.Zero(t1)
	}
	return (c.Zero(t2)*t2 - c.Zero(t1)*t1) / (t2 - t1)
}

// Tenors returns the pillar tenors (ascending). The returned slice is a copy.
func (c *Curve) Tenors() []float64 {
	out := make([]float64, len(c.tenors))
	copy(out, c.tenors)
	return out
}

// Zeros returns the continuously-compounded zero rate at each pillar. Copy.
func (c *Curve) Zeros() []float64 {
	out := make([]float64, len(c.zeros))
	copy(out, c.zeros)
	return out
}
