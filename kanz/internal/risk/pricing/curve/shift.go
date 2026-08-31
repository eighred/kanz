package curve

import "fmt"

// Yield-curve shift primitives (FI-01e) — the curve-shape stress inputs the
// scenario library (parallel / steepener / butterfly) is built from. This lands
// the MODEL-01 curve-shift deferral: a Shift transforms a base Curve into a
// shocked one, repriced against to capture a bond book's rate sensitivity beyond
// a single parallel move.
//
// Magnitudes are in basis points (1 bp = 1e-4 in the zero rate). Each Apply
// returns a NEW curve; the base is never mutated — the pure-clone scenario
// stance.

// bp converts basis points to an absolute rate change.
const bp = 1e-4

// Shift transforms a base curve into a shocked curve.
type Shift interface {
	// Apply returns a new curve with the shift added to each pillar's zero rate.
	Apply(*Curve) *Curve
	// Description is a short human-readable label for audit/scenario naming.
	Description() string
}

// shiftCurve rebuilds c with delta(tenor) added to each pillar's continuous zero
// rate. Pillars (and interpolation) are preserved; only the levels move.
func shiftCurve(c *Curve, delta func(tenor float64) float64) *Curve {
	zeros := make([]float64, len(c.zeros))
	for i, t := range c.tenors {
		zeros[i] = c.zeros[i] + delta(t)
	}
	// Inputs are already continuous and ascending — reuse the validated copy.
	out := &Curve{
		tenors: append([]float64(nil), c.tenors...),
		zeros:  zeros,
		interp: c.interp,
		// The strip coverage travels with the shock (#908). A stress applied to a
		// curve that lost its long end produces a SHOCKED curve that is short of
		// exactly the same instruments, and a scenario result is where that
		// matters most — the shift path must not launder the gap into an UNKNOWN.
		coverage: c.coverage,
	}
	return out
}

// Parallel shifts every tenor by the same basis-point amount — the textbook
// "rates up 100bp" stress.
type Parallel struct{ Bp float64 }

// Apply implements Shift.
func (s Parallel) Apply(c *Curve) *Curve {
	d := s.Bp * bp
	return shiftCurve(c, func(float64) float64 { return d })
}

// Description implements Shift.
func (s Parallel) Description() string { return fmt.Sprintf("parallel %+.0fbp", s.Bp) }

// Steepener tilts the curve: it ramps LINEARLY IN TENOR from ShortBp at the
// shortest pillar to LongBp at the longest. A bear steepener is {ShortBp:0,
// LongBp:+50}; a bull steepener {ShortBp:-50, LongBp:0}; a pivot steepener
// {ShortBp:-25, LongBp:+25}. The differentiated move a Parallel cannot express.
type Steepener struct{ ShortBp, LongBp float64 }

// Apply implements Shift.
func (s Steepener) Apply(c *Curve) *Curve {
	tShort, tLong := c.tenors[0], c.tenors[len(c.tenors)-1]
	span := tLong - tShort
	return shiftCurve(c, func(t float64) float64 {
		if span <= 0 {
			return s.ShortBp * bp
		}
		w := (t - tShort) / span
		return (s.ShortBp + w*(s.LongBp-s.ShortBp)) * bp
	})
}

// Description implements Shift.
func (s Steepener) Description() string {
	return fmt.Sprintf("steepener %+.0f→%+.0fbp", s.ShortBp, s.LongBp)
}

// Butterfly bumps the belly relative to the wings: a triangular weight peaking
// at the median tenor (BellyBp) and falling to −BellyBp/2 at both ends, so the
// move is curvature with little net level/slope. Positive BellyBp is a "belly
// cheapens" (yields up in the middle) butterfly.
type Butterfly struct{ BellyBp float64 }

// Apply implements Shift.
func (s Butterfly) Apply(c *Curve) *Curve {
	tShort, tLong := c.tenors[0], c.tenors[len(c.tenors)-1]
	mid := 0.5 * (tShort + tLong)
	half := 0.5 * (tLong - tShort)
	return shiftCurve(c, func(t float64) float64 {
		if half <= 0 {
			return s.BellyBp * bp
		}
		// Triangle: 1 at mid, −0.5 at the wings.
		tri := 1 - 1.5*absf(t-mid)/half
		return s.BellyBp * tri * bp
	})
}

// Description implements Shift.
func (s Butterfly) Description() string { return fmt.Sprintf("butterfly %+.0fbp", s.BellyBp) }

func absf(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// WithPillarBump returns a copy of c with pillar i's continuous zero rate moved
// by delta (absolute, e.g. 1e-4 for +1bp). Because the curve interpolates
// linearly between pillars, a single-pillar bump is exactly the tent-shaped
// localized perturbation a key-rate duration is defined against (FI-01c). i out
// of range ⇒ an unbumped copy.
func (c *Curve) WithPillarBump(i int, delta float64) *Curve {
	zeros := append([]float64(nil), c.zeros...)
	if i >= 0 && i < len(zeros) {
		zeros[i] += delta
	}
	return &Curve{
		tenors:   append([]float64(nil), c.tenors...),
		zeros:    zeros,
		interp:   c.interp,
		coverage: c.coverage, // a bumped curve is short of what its base was short of
	}
}
