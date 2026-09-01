package marketview

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/marketedge/volprofile"
)

// A SHAPE IS A RESOLVED PROFILE, AND IT IS WHERE THE INTEGRATION LIVES (#897).
//
// There are two ways a volume profile reaches an algorithm on this platform and
// there must be exactly ONE arithmetic between them.
//
//	Volume  resolves a shape per question from a live volprofile.Store — the
//	        market-data edge, where the fold is maintained.
//	Pinned  holds ONE published, versioned shape that crossed the bus as a FACT
//	        — the OMS, where a parent order's schedule is derived and re-derived.
//
// The second exists because the first cannot answer the OMS's question. A store
// is per-process and mutates as sessions complete, so two pods asking it the same
// question get two answers, and services/oms/internal/order.authorizeChild
// compares a child's quantity as an EXACT RATIONAL — a difference in the last
// digit refuses a legitimate slice as a forgery.
//
// WHAT MUST NOT DIFFER IS THE ARITHMETIC. If the store-backed view and the
// pinned view integrated a curve differently, a schedule planned on the edge and
// re-derived in the OMS would disagree for a reason nothing in either place
// names. So both resolve to this type and call expectedOver, which is the only
// implementation.

// ErrShape refuses a shape that could never answer anything. It is ErrConfig's
// sibling for a resolved curve rather than for a view's wiring.
var ErrShape = errors.New("marketview: shape")

// Shape is a measured intraday curve for ONE instrument, resolved.
//
// # Expected is a QUANTITY PER BUCKET, not a dimensionless share
//
// volprofile.Answer keeps the two apart — Shares sums to 1 and says WHEN, and
// SessionVolume says HOW MUCH — because only one of them is dimensionless and a
// participation cap is a fraction OF a quantity. Every consumer of that pair
// multiplies them together immediately: VWAP needs the ratios between buckets,
// POV needs the absolute level, and both get exactly the product.
//
// So the product is formed ONCE, where a shape is resolved, and the arithmetic
// below never sees a share again. It is also what crosses the wire (#897's
// market.v1.VolumeProfile.expected_volume), so the FACT and this type carry the
// same thing and a pinned schedule needs no reconstruction step that a store-fed
// one does not have.
type Shape struct {
	// InstrumentID is what this shape describes. A view built on it answers
	// UNKNOWN about every other instrument rather than about this one.
	InstrumentID string

	// Bucket is the width each entry of Expected covers.
	Bucket time.Duration

	// Horizon is how far back the sessions behind this shape were retained.
	//
	// IT TRAVELS WITH THE SHAPE. An interval longer than the history behind the
	// curve is an extrapolation past everything measured, and the refusal must be
	// a property of the DATUM rather than of whichever reader happens to hold it:
	// a horizon read from each pod's own configuration would let two pods disagree
	// about a schedule derived from one order.
	Horizon time.Duration

	// Expected[i] is the quantity expected to trade in intraday bucket i of a
	// typical session. Never nil for a usable shape.
	Expected []*big.Rat
}

// fromAnswer resolves a volprofile.Answer into a Shape, or reports UNKNOWN.
//
// EVERY NON-KNOWN VERDICT BECOMES ok=false AND THERE IS NO PATH THAT RETURNS A
// ZERO. A series nobody has folded, a history shorter than the desk requires and
// a shape past the retention horizon are all UNKNOWN, and so is a KNOWN shape
// with no level: the shares would answer "which half of the day is busier"
// perfectly well, and multiplying them by nothing would answer "how much" with a
// zero.
func fromAnswer(a volprofile.Answer, instrumentID string, horizon time.Duration) (Shape, bool) {
	if !a.Known() || len(a.Shares) == 0 || a.Bucket <= 0 {
		return Shape{}, false
	}
	if a.SessionVolume == nil || a.SessionVolume.Sign() <= 0 {
		return Shape{}, false
	}
	exp := make([]*big.Rat, len(a.Shares))
	for i, sh := range a.Shares {
		exp[i] = new(big.Rat).Mul(sh, a.SessionVolume)
	}
	return Shape{InstrumentID: instrumentID, Bucket: a.Bucket, Horizon: horizon, Expected: exp}, true
}

// validate refuses a shape nothing could be scheduled against.
//
// LOUD AT CONSTRUCTION RATHER THAN UNKNOWN AT DECISION TIME, which is the rule
// ErrConfig already states for a view's wiring: a malformed shape and a market
// nobody measured both produce "no volume profile" at the client, and an operator
// would go looking at the feed while the fault was in a decoder.
func (s Shape) validate() error {
	switch {
	case s.InstrumentID == "":
		return fmt.Errorf("%w: names no instrument, so every question it answered would be "+
			"about nothing", ErrShape)
	case s.Bucket <= 0:
		return fmt.Errorf("%w: bucket width %s is not positive", ErrShape, s.Bucket)
	case volprofile.Session%s.Bucket != 0:
		return fmt.Errorf("%w: bucket %s does not divide the %s session evenly, so the last bin "+
			"of every session would be short and its share understated", ErrShape, s.Bucket, volprofile.Session)
	case len(s.Expected) != volprofile.BucketsPerSession(s.Bucket):
		return fmt.Errorf("%w: %d buckets for a %s bin over a %s session, which needs %d — a curve "+
			"that does not cover the session would size some intervals from bins that are not there",
			ErrShape, len(s.Expected), s.Bucket, volprofile.Session, volprofile.BucketsPerSession(s.Bucket))
	case s.Horizon < 0:
		return fmt.Errorf("%w: horizon %s is negative", ErrShape, s.Horizon)
	}
	for i, e := range s.Expected {
		if e == nil {
			return fmt.Errorf("%w: bucket %d carries no value — a nil is not a measured zero and "+
				"must not become one", ErrShape, i)
		}
		if e.Sign() < 0 {
			return fmt.Errorf("%w: bucket %d expects %s, and a negative quantity is not a market",
				ErrShape, i, e.RatString())
		}
	}
	return nil
}

// expectedOver is the quantity expected to trade in [from, to).
//
// # The interval is INTEGRATED, not looked up
//
// A slice's interval is whatever the parent's window divided by its slice count
// happens to be, and it will not line up with the profile's bins. So the answer
// is the cumulative curve evaluated at both ends: whole sessions between them
// contribute a session's total each, and each end contributes the bins it fully
// covers plus a PRO-RATA part of the bin it lands inside.
//
// THE PRO-RATA IS AN ASSUMPTION AND IT IS STATED: volume is taken as uniform
// WITHIN a bin, because a bin is the finest thing the profile measured and
// anything smaller is a shape nobody sampled. It is the same assumption a caller
// makes by choosing the bin width, applied consistently rather than by rounding
// an interval to the nearest bin — which would put two different slices of the
// same parent on the same answer and size them identically.
func expectedOver(s Shape, from, to time.Time) (*big.Rat, bool) {
	from, to = from.UTC(), to.UTC()
	if to.Before(from) {
		// A backwards interval is not a question about this market. It is refused
		// as UNKNOWN rather than answered with a negative or an absolute value,
		// either of which would size a child from a caller's own bug.
		return nil, false
	}
	// A WINDOW LONGER THAN THE HISTORY THE SHAPE WAS BUILT FROM. Beyond it the
	// answer is an extrapolation of a mean over sessions nobody sampled, and this
	// package does not extrapolate.
	if s.Horizon > 0 && to.Sub(from) > s.Horizon {
		return nil, false
	}
	if len(s.Expected) == 0 || s.Bucket <= 0 {
		return nil, false
	}

	// WHOLE SESSIONS BETWEEN THE TWO ENDS, counted from the truncated boundaries
	// rather than from the raw difference: [23:00 Monday, 01:00 Tuesday) spans two
	// hours and one session boundary, and only the boundary count is the number of
	// complete curves between the prefixes below.
	sessions := to.Truncate(volprofile.Session).Sub(from.Truncate(volprofile.Session)) / volprofile.Session

	out := new(big.Rat)
	if sessions != 0 {
		whole := new(big.Rat)
		for _, e := range s.Expected {
			whole.Add(whole, e)
		}
		out.Mul(whole, new(big.Rat).SetInt64(int64(sessions)))
	}
	out.Add(out, prefixExpected(s, to))
	out.Sub(out, prefixExpected(s, from))
	if out.Sign() < 0 {
		return nil, false
	}
	return out, true
}

// prefixExpected is the quantity accumulated from a session's start up to t.
func prefixExpected(s Shape, t time.Time) *big.Rat {
	off := t.Sub(t.Truncate(volprofile.Session))
	if off < 0 {
		return new(big.Rat)
	}

	full := int(off / s.Bucket)
	out := new(big.Rat)
	for i := 0; i < full && i < len(s.Expected); i++ {
		out.Add(out, s.Expected[i])
	}
	if full >= len(s.Expected) {
		return out
	}

	// THE PART-BIN, PRO-RATA. Exact rationals throughout: the remainder and the
	// bin are both integer nanosecond counts, so the fraction is exact and a
	// schedule derived from it is reproducible to the last digit on every pod.
	rem := off - time.Duration(full)*s.Bucket
	if rem <= 0 {
		return out
	}
	part := new(big.Rat).SetFrac64(int64(rem), int64(s.Bucket))
	return out.Add(out, part.Mul(part, s.Expected[full]))
}
