package algo

import (
	"errors"
	"fmt"
	"math/big"
	"time"
)

// The machinery the volume-driven algorithms share (#869).
//
// # What a volume-driven schedule needs that TWAP does not
//
// TWAP is a function of the parent alone. VWAP and POV are functions of the
// parent AND of what the market is expected to do, so they need three things
// TWAP never asks for: which instrument (Plan.InstrumentID), what is expected to
// trade in each slice's own interval (MarketView.ExpectedVolume over
// [boundary(i), boundary(i+1))), and an answer to what happens when the view
// cannot say.
//
// The third is the whole reason this file exists separately from the arithmetic.
//
// # UNKNOWN IS A REFUSAL, AND IT IS THE POINT OF #869
//
// #867's volume profile answers UNKNOWN — for a series nobody has measured, for
// one whose history is shorter than the desk requires, and for one whose newest
// session is past the retention horizon. Every one of those reaches an algorithm
// here as `known=false`, and the ONLY correct response is to refuse the schedule.
//
// The tempting alternative is to fall back to equal shares. It is well-formed, it
// is in range, and every arithmetic below works on it. It is also EXACTLY TWAP —
// so a VWAP order scheduled that way becomes a TWAP order wearing the wrong name,
// and #866's execution attribution would decompose the shortfall of an algorithm
// that never ran. That is worse than a refusal, because a refusal is visible: the
// order does not exist, the desk is told why, and nothing was traded under a
// label nobody can defend.
//
// So there is no fallback anywhere in this file, and ErrVolumeUnknown is its own
// sentinel so the OMS can answer it with its own refusal code rather than folding
// it into "your numbers are wrong".

var (
	// ErrNoInstrument refuses a volume-driven plan that names no instrument.
	//
	// The view is asked ABOUT something. An empty instrument would be a question
	// about nothing, and whatever a view answered to it would size real children.
	ErrNoInstrument = errors.New("algo: a volume-driven schedule must name the instrument it works")

	// ErrVolumeUnknown refuses a schedule the market view cannot size.
	//
	// A DISTINCT SENTINEL BECAUSE IT IS A DISTINCT REFUSAL, in exactly the sense
	// ErrUnknownAlgo is. ErrEmptyWindow and ErrCapUnsatisfiable are the operator's
	// own arithmetic coming back at them and they fix them by changing the
	// command. This one says the command is fine and NOTHING HERE CAN SEE THE
	// MARKET — an absent profile, a stale one, or an order path that carries no
	// market data at all. The two need different answers from the desk, so they
	// carry different codes.
	ErrVolumeUnknown = errors.New("algo: expected volume is UNKNOWN, and a volume-driven schedule must not be invented")

	// ErrNoVolumeInBucket refuses a schedule with a slice nothing is expected to
	// trade in.
	//
	// IT IS A REFUSAL RATHER THAN A ZERO-SIZED CHILD, and rather than a schedule
	// with that index left out, because BOTH of those leave a parent resting
	// forever. A child of zero quantity is not placeable at any venue this
	// platform reaches; a schedule that omits index i can never satisfy
	// services/oms/internal/schedule.Complete, which asks whether every index in
	// [0, Slices) exists. The remedy is the operator's and the refusal names it:
	// fewer slices, or a window over hours this instrument actually trades in.
	ErrNoVolumeInBucket = errors.New("algo: no volume is expected in a slice's own interval")
)

// expectedVolumes asks the view what trades in each slice's interval.
//
// ONE CALL PER SLICE, OVER THE SLICE'S OWN [boundary(i), boundary(i+1)), so the
// interval a child is sized against is the interval it is sent in. The boundaries
// come from Plan.boundary rather than from a step computed here, for the reason
// that function gives: a second copy of the expression would put the due time and
// the sizing interval one rounding apart.
//
// EVERY REFUSAL IS RAISED ON THE FIRST SLICE THAT TRIPS IT, and names which slice
// and which interval — an operator reading "expected volume is UNKNOWN" with no
// interval cannot tell a profile that does not exist from a window that runs past
// the end of one.
func expectedVolumes(p Plan, mkt MarketView) ([]*big.Rat, *big.Rat, error) {
	if p.InstrumentID == "" {
		return nil, nil, ErrNoInstrument
	}
	if mkt == nil {
		// Run normalises this, but an algorithm reached directly must not
		// dereference a nil view. Absence and "I know nothing" are the same answer.
		mkt = UnknownMarket{}
	}

	vols := make([]*big.Rat, p.Slices)
	total := new(big.Rat)
	for i := range p.Slices {
		from, to := p.boundary(i), p.boundary(i+1)
		v, known := mkt.ExpectedVolume(p.InstrumentID, from, to)
		if !known {
			return nil, nil, fmt.Errorf("%w: %s over [%s, %s), which is slice %d of %d",
				ErrVolumeUnknown, p.InstrumentID,
				from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), i, p.Slices)
		}
		if v == nil || v.Sign() <= 0 {
			return nil, nil, fmt.Errorf("%w: %s over [%s, %s), which is slice %d of %d — a child "+
				"of zero quantity cannot be placed and a schedule missing that index can never "+
				"complete, so this plan is refused whole; work fewer slices, or a window this "+
				"instrument trades in",
				ErrNoVolumeInBucket, p.InstrumentID,
				from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), i, p.Slices)
		}
		// THE VIEW'S VALUE IS COPIED. big.Rat is a pointer and a view that hands
		// out its own cached one would have every later reader's shape mutated by
		// the arithmetic below.
		vols[i] = new(big.Rat).Set(v)
		total.Add(total, vols[i])
	}
	return vols, total, nil
}

// allocateProportional divides the parent across the slices in proportion to the
// volume expected in each.
//
// CONSERVATION IS EXACT AND STRUCTURAL, not asserted afterwards: slice i is
// Total·vᵢ/V and the vᵢ sum to V, so the children sum to Total with no remainder
// — as rationals, with no rounding anywhere. That is the property #435 named
// first, and the one a decimal implementation would lose: 0.0000001 left unworked
// is a parent that never completes.
//
// The cap is checked BEFORE any slice is produced and the plan is refused WHOLE,
// which is TWAP's rule and for TWAP's reason. Trimming the offending child would
// leave the remainder unworked; spreading it over the others would put quantity
// into intervals the market is not expected to absorb it in, which is the one
// thing a volume-driven schedule exists to avoid.
func allocateProportional(p Plan, vols []*big.Rat, total *big.Rat) ([]Slice, error) {
	out := make([]Slice, 0, p.Slices)
	for i := range p.Slices {
		q := new(big.Rat).Mul(p.Total, vols[i])
		q.Quo(q, total)

		if p.MaxSlice != nil && p.MaxSlice.Sign() > 0 && q.Cmp(p.MaxSlice) > 0 {
			return nil, fmt.Errorf("%w: slice %d of %d is %s against a cap of %s — the volume "+
				"expected in that interval is %s of %s across the window, and a volume-driven "+
				"schedule cannot move the excess without sending it where the market is not "+
				"expected to absorb it",
				ErrCapUnsatisfiable, i, p.Slices, q.FloatString(8), p.MaxSlice.FloatString(8),
				vols[i].RatString(), total.RatString())
		}

		out = append(out, Slice{
			Index:    i,
			Due:      p.boundary(i),
			Quantity: q,
		})
	}
	return out, nil
}
