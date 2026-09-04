// Package algo works a parent order as a schedule of child orders (#435).
//
// # The gap this closes
//
// Every order on this platform is sent to a venue WHOLE. The signal fan-out
// splits by venue allocation weight — a 60/40 split of a 100 BTC signal is two
// orders, not two hundred slices — and nothing between the OMS and the venue
// slices a parent over time. So SIZE IS SLIPPAGE: the platform computes market
// impact in internal/risk/liquidity and the execution path cannot act on it.
//
// #240 is on the tracker because a pct_of_equity alert walked past a quantity cap
// and fanned out 1,800 BTC. That order would have gone to one venue in one
// message.
//
// # The schedule is DERIVED, never stored
//
// #435 names the trap directly: "An algo that keeps its schedule in memory turns
// a pod restart into an abandoned parent order with children half-sent … The
// schedule must be durable from slice 1, or slice 1 is not done."
//
// The answer here is not a schedule table. Plan is a pure function of fields the
// parent order ALREADY stores durably — quantity, window, slice count — so the
// schedule is recomputed identically on every pod, on every restart, forever.
// There is no in-memory schedule to lose, and no second store to fall out of
// sync with the order.
//
// What has actually been SENT is durable too, and separately: a child is an
// order, and the orders that exist are the ones that were sent. So "where are
// we" is answered by the order store rather than by a cursor somebody has to
// keep.
//
// # Deterministic, and therefore provable without a venue
//
// Same inputs, same schedule, every time — no clock read inside, no randomness,
// no venue. That is what lets this be asserted closed-form, which is why #435
// sequences TWAP first: it is the one algo whose expected output is an exact
// value rather than a distribution.
package algo

import (
	"errors"
	"fmt"
	"math/big"
	"math/bits"
	"time"
)

var (
	// ErrEmptyQuantity rejects a plan with nothing to work.
	ErrEmptyQuantity = errors.New("algo: total quantity must be positive")
	// ErrEmptyWindow rejects a window that does not move forward. A zero-length
	// window is not "send it all now" — it is a schedule nobody specified, and
	// guessing which they meant is how a 1,800 BTC order goes out in one message.
	ErrEmptyWindow = errors.New("algo: the working window must end after it starts")
	// ErrNoSlices rejects a plan with no slices.
	ErrNoSlices = errors.New("algo: slice count must be positive")
	// ErrCapUnsatisfiable is returned when the requested slice count cannot honour
	// the participation cap. See Plan.MaxSlice.
	ErrCapUnsatisfiable = errors.New("algo: slice count cannot honour the participation cap")
)

// validate checks the three things every algorithm needs before it can produce a
// single child, in the order TWAP has always checked them.
//
// SHARED SO THE THREE ALGORITHMS REFUSE THE SAME PLAN THE SAME WAY. A second
// copy of these checks is a second answer to "is this schedule workable", and the
// two would drift the first time one of them grew a case — with the failure that
// an order admitted under one algorithm is refused after being switched to
// another, for a reason nothing in the command changed.
func (p Plan) validate() error {
	if p.Total == nil || p.Total.Sign() <= 0 {
		return ErrEmptyQuantity
	}
	if p.Slices <= 0 {
		return ErrNoSlices
	}
	if !p.End.After(p.Start) {
		return fmt.Errorf("%w: [%s, %s]", ErrEmptyWindow,
			p.Start.UTC().Format(time.RFC3339), p.End.UTC().Format(time.RFC3339))
	}
	return nil
}

// boundary is the instant slice i starts, for i in [0, Slices]. boundary(0) is
// Start and boundary(Slices) is End exactly.
//
// INTEGER NANOSECOND ARITHMETIC ON THE OFFSET, not repeated addition of a step:
// accumulating a rounded step drifts, and the last child of a long schedule would
// fall outside the window it was supposed to finish in. It is one function
// because a volume-driven algorithm needs both ENDS of a slice's interval to ask
// the market what trades in it, and a second copy of this expression would put
// the schedule's due times and the interval it was sized against one rounding
// apart — the child would be sized for a window it is not sent in.
// THE PRODUCT IS COMPUTED IN 128 BITS, AND THAT IS NOT A MICRO-OPTIMISATION
// (#898). `int64(span) * int64(i)` is nanoseconds times a slice index, and it
// overflows int64 far inside the range of a uint32 slice_count that arrives on
// the wire:
//
//	window     overflows at
//	   1h      i > 2_562_047
//	  24h      i >   106_751     ← a day-long parent at ~one child per 0.8s
//
// The threshold scales INVERSELY with the window, so the longer the parent is
// worked the fewer slices it takes — which is backwards from the intuition that
// a long window is the safe case.
//
// What overflow does here is worse than a wrong number. The product goes
// NEGATIVE, so offset is negative, so p.Start.Add(offset) lands BEFORE the
// window starts — and Due() filters on "at or before now", so every child of the
// schedule reads as due immediately. A parent somebody asked to be worked
// carefully over a day goes to the venue in one pass. That is the exact failure
// the window's own proto comment says a zero-length window must not be allowed
// to cause, arrived at by arithmetic instead.
//
// bits.Mul64/Div64 gives the exact quotient with no allocation and no float.
// Div64 panics when the high word is >= the divisor; that cannot happen here
// because callers only ever ask for i in [0, Slices], so span·i < span·Slices and
// the quotient is bounded by span itself. requireSaneSlices, and the OMS's
// admission bound above it, are what keep i in range — this is the arithmetic
// being correct for every i it is actually given, not a substitute for them.
func (p Plan) boundary(i int) time.Time {
	span := p.End.Sub(p.Start)
	hi, lo := bits.Mul64(uint64(span), uint64(i))
	q, _ := bits.Div64(hi, lo, uint64(p.Slices))
	offset := time.Duration(q)
	return p.Start.Add(offset).UTC()
}

// Interval is the window slice i is worked over: [boundary(i), boundary(i+1)).
//
// # It is exported so the MEASUREMENT can name the same intervals the SCHEDULE
// # used
//
// A realised-participation figure (#1007) divides what a child filled by what
// actually printed in that child's own interval, and the OMS computes it after
// the parent is terminal. Reconstructing the boundaries there — `start +
// i*(end-start)/slices` written a second time — would put the denominator's
// window and the schedule's due times one rounding apart, which is the exact
// defect boundary's own comment exists to prevent, moved one package away and
// out of sight of the tests that hold it here.
//
// ok is false for an index outside [0, Slices), rather than a clamped or
// extrapolated window. A caller asking about a slice that does not exist has
// already lost track of the schedule, and answering with the last interval would
// attribute one child's fills to another child's minutes.
//
// IT READS NO CLOCK AND HOLDS NO STATE, like everything else in this package.
// The interval is a function of the parent's durable window and slice count, so
// two pods measuring the same finished parent measure it over the same minutes.
func (p Plan) Interval(i int) (from, to time.Time, ok bool) {
	if p.Slices <= 0 || i < 0 || i >= p.Slices || !p.End.After(p.Start) {
		return time.Time{}, time.Time{}, false
	}
	return p.boundary(i), p.boundary(i + 1), true
}

// Plan is everything needed to work a parent order, and every field of it lives
// on the parent order durably. Nothing here is state.
type Plan struct {
	// Algo names the algorithm that works this parent — order.v1's
	// ExecutionSchedule.algo, which the order stores durably like every other
	// field here.
	//
	// UNSET IS REFUSED BY Run, never defaulted. It is deliberately a name rather
	// than a resolved implementation so that Plan stays a value derived entirely
	// from the order: two pods holding the same order hold the same Plan, and the
	// registry is what turns the name into code (see Lookup).
	//
	// TWAP, the free function below, does not read it — it IS the TWAP
	// arithmetic, and Run is what selects. A caller that reaches TWAP directly has
	// chosen the algorithm at the call site rather than from the order, which is
	// what the closed-form tests do on purpose and what nothing on the order path
	// does.
	Algo Name

	// InstrumentID is what the parent trades, and it is here so a volume-driven
	// algorithm can ask MarketView about the right instrument.
	//
	// TWAP DOES NOT READ IT, so it is unset on every plan this platform derived
	// before #869 and nothing about those schedules changes. VWAP and POV REFUSE
	// an empty one rather than asking the view about "": a view that answered a
	// nameless instrument would be answering about nothing, and the shape it
	// returned would size real children.
	InstrumentID string

	// Total is the parent's ordered quantity — the amount the children must sum
	// to exactly.
	Total *big.Rat

	// Start and End bound the working window. Slice i is due at
	// Start + i·(End−Start)/Slices, so the FIRST slice is due at Start.
	Start, End time.Time

	// Slices is how many children the parent is worked as.
	Slices int

	// MaxSlice caps any single child's quantity — the participation cap in its
	// schedule-time form. Nil means uncapped.
	//
	// IT IS A SIZE, NOT A FRACTION OF VOLUME, and that is a deliberate limitation
	// stated rather than hidden. A true participation cap is a fraction of the
	// volume that trades WHILE the order works, which is not knowable when the
	// schedule is computed — it is knowable per bar, at send time, and that is
	// POV (#435's slice 3), not TWAP. What this bounds is the largest single
	// message this platform will put in front of a venue, which is the half that
	// can be decided in advance and the half #240 needed.
	MaxSlice *big.Rat

	// MaxParticipation is the hard participation cap: the largest fraction of the
	// volume expected to trade in a slice's interval that a child may be. Nil
	// means no cap was given, which POV REFUSES rather than reading as uncapped.
	//
	// A FRACTION, WHERE MaxSlice IS A SIZE, and the two bound different things. A
	// size cap bounds the largest single message this platform puts in front of a
	// venue; this bounds how much of the tape that message IS. On a thin
	// instrument a perfectly small order is still the whole print, and only this
	// one can see that — which is why #869 puts the cap in the execution
	// vocabulary rather than leaving "never more than n% of volume" as a desk
	// convention nothing enforces.
	//
	// IT IS BOUNDED ABOVE BY 1 AND BELOW BY 0, EXCLUSIVE. A cap of 0 permits no
	// child at all and a cap above 1 permits more than everything that trades —
	// both are spellings of "no cap" that read as a configured control, which is
	// the shape this platform refuses everywhere.
	MaxParticipation *big.Rat
}

// Slice is one child order: how much, and when it becomes due.
type Slice struct {
	Index    int
	Due      time.Time
	Quantity *big.Rat
}

// TWAP divides the parent evenly across the window.
//
// EVENLY IN QUANTITY, EVENLY IN TIME, and exactly — every slice is Total/Slices
// as an exact rational, so the children sum to Total with no remainder to lose.
// That matters more than it looks: a schedule that divides 10 into 3 and rounds
// each child leaves 0.0000001 unworked forever, and the parent never completes.
// #435's first assertion is conservation for exactly this reason.
//
// The cap is checked BEFORE any slice is produced. A plan that cannot honour it
// is refused whole rather than trimmed: silently shrinking children would leave
// the remainder unworked, and silently adding slices would change the schedule
// the operator asked for into one nobody chose. Both are the "control that
// reports success" this platform designs against.
func TWAP(p Plan) ([]Slice, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}

	each := new(big.Rat).Quo(p.Total, new(big.Rat).SetInt64(int64(p.Slices)))

	if p.MaxSlice != nil && p.MaxSlice.Sign() > 0 && each.Cmp(p.MaxSlice) > 0 {
		// The minimum slice count that would honour the cap, so the refusal tells
		// the operator what to change rather than only that something is wrong.
		need := new(big.Rat).Quo(p.Total, p.MaxSlice)
		return nil, fmt.Errorf("%w: %d slices of %s exceeds the cap of %s; needs at least %d",
			ErrCapUnsatisfiable, p.Slices, each.FloatString(8), p.MaxSlice.FloatString(8),
			ceilRat(need))
	}

	out := make([]Slice, 0, p.Slices)
	for i := range p.Slices {
		out = append(out, Slice{
			Index:    i,
			Due:      p.boundary(i),
			Quantity: new(big.Rat).Set(each),
		})
	}
	return out, nil
}

// Due returns the slices of a plan that are due at or before now — the ones a
// driver should have sent.
//
// IT IS A FILTER OVER THE WHOLE SCHEDULE, NOT A CURSOR. A cursor is state, and
// state is what gets lost on restart; recomputing the full schedule and filtering
// it is what makes a pod that has just booted reach the same answer as one that
// has been running all day. Which of these have actually been sent is the order
// store's answer, not this package's.
func Due(slices []Slice, now time.Time) []Slice {
	out := make([]Slice, 0, len(slices))
	for _, s := range slices {
		if !s.Due.After(now.UTC()) {
			out = append(out, s)
		}
	}
	return out
}

// Sum totals a schedule's quantities. Used by the conservation check, and by any
// caller that wants to assert the same property at a different layer.
func Sum(slices []Slice) *big.Rat {
	total := new(big.Rat)
	for _, s := range slices {
		total.Add(total, s.Quantity)
	}
	return total
}

// ceilRat returns the smallest integer >= r, for the "needs at least N slices"
// half of the cap refusal.
func ceilRat(r *big.Rat) int {
	q := new(big.Int).Quo(r.Num(), r.Denom())
	if new(big.Int).Mul(q, r.Denom()).Cmp(r.Num()) != 0 {
		q.Add(q, big.NewInt(1))
	}
	return int(q.Int64())
}
