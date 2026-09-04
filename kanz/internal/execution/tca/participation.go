package tca

import (
	"math/big"
	"time"
)

// REALISED PARTICIPATION: what fraction of the tape a decision actually was
// (#1007).
//
// # The half of the participation control that was never measured
//
// EXECUTION_ALGO_POV refuses a parent that its volume profile's FORECAST says
// cannot be worked inside max_participation_rate. internal/execution/algo/pov.go
// states the limit of that plainly — "if the realised tape comes in thinner than
// the profile, the children were sized for volume that did not arrive. Nothing
// here can see that, and nothing here pretends to." Nothing elsewhere saw it
// either: no metric, no FACT and no post-trade record carried a participation
// figure, so a cap of 0.08 read from every screen as a rate that was honoured
// with nothing anywhere able to confirm or contradict it.
//
// This is the confirmation. It is measured AFTER the fact, from the children's
// fills and the tape as it actually printed, and it decides nothing: a decision
// this refused or rerouted would put an analytic on the capital path, which is
// the same stance Measure and Attribute take.
//
// # WHY IT LIVES HERE AND NOT IN internal/execution/algo
//
// An algorithm in that package is a pure function of the parent's durable
// fields, and test/arch/schedule_is_derived_test.go holds it there: two pods
// must derive the same children forever, so nothing in it may read a clock, a
// fill or a tape. This reads all three. The guard's own doc names the split —
// "the point is that ONE package is pure, so a driver elsewhere may hold
// whatever it needs" — and this is the measurement half of that sentence.
//
// # THE RATE IS A LOWER BOUND, AND THAT IS THE SOUND DIRECTION
//
// The only realised-volume record this platform keeps is the 1-minute candle
// series, so a slice interval that does not begin and end on a minute boundary
// is measured against the candles COVERING it: a window at least as long, hence
// a volume at least as large, hence a rate no larger than the true one. So an
// exceedance reported here is real, and a rate under the cap is not proof the
// cap held. That asymmetry is deliberate and it is the estate's standing
// doctrine for interval evidence — internal/marketedge/coverage credits a floor
// of observed time, and internal/marketdata/store/gaps.go rules that a missing
// bucket proves a window cannot support a claim while a whole window proves
// nothing.
//
// The bound belongs to the SOURCE rather than to this arithmetic, which divides
// exactly whatever it is handed. It is written here because this is where a
// reader meets the number.

// RealisedVolume answers what actually PRINTED, as far as the caller can vouch
// for it.
//
// known=false IS UNKNOWN AND IS NEVER ZERO, the same three-value shape
// algo.MarketView carries for the forecast. A minute in which nothing traded
// produces no candle, and internal/marketedge/bars is explicit that its producer
// "cannot tell 'nothing traded' from 'the feed was down' or 'we were rolling
// pods'". So an interval with nothing behind it must arrive here as UNKNOWN: a
// zero denominator is not a very large participation rate, it is no rate, and
// scoring it as either would be loudest in exactly the thin market the cap
// exists for.
type RealisedVolume interface {
	// Volume is the quantity that printed in [from, to) on one venue's book.
	// known=false ⇒ this source cannot say, which includes an interval it was
	// not yet observing.
	Volume(instrumentID, venue string, from, to time.Time) (qty *big.Rat, known bool)
}

// ParticipationQuality says whether the measurement could be made at all.
//
// It mirrors order.v1.ParticipationQuality one for one, and the mapping to the
// wire lives at the OMS boundary rather than here — this package knows nothing
// about the FACT, exactly as Quality knows nothing about AttributionQuality.
type ParticipationQuality int

const (
	// ParticipationUnknown is the zero value and is never returned. It exists so
	// a Participation that was never computed cannot be read as one that was and
	// came back empty.
	ParticipationUnknown ParticipationQuality = iota
	// ParticipationNotWorked: the decision had no intervals — it was not worked
	// as a schedule. Not a gap in the data.
	ParticipationNotWorked
	// ParticipationMeasured: every interval the decision traded in had realised
	// volume behind it.
	ParticipationMeasured
	// ParticipationPartial: some did and some did not. The rates cover the
	// observed intervals only.
	ParticipationPartial
	// ParticipationUnobservable: none did. Both rates are nil.
	ParticipationUnobservable
)

func (q ParticipationQuality) String() string {
	switch q {
	case ParticipationNotWorked:
		return "NOT_WORKED"
	case ParticipationMeasured:
		return "MEASURED"
	case ParticipationPartial:
		return "PARTIAL"
	case ParticipationUnobservable:
		return "UNOBSERVABLE"
	default:
		return "UNKNOWN"
	}
}

// Interval is one slice of a worked decision: the window it was worked over, and
// what the order that carried it filled.
//
// From and To are the SCHEDULE's own boundaries (algo.Plan.Interval), not times
// derived a second time by the caller. Filled is that child's filled quantity;
// nil or non-positive means the slice traded nothing.
type Interval struct {
	Index int
	// Venue is the book this slice was actually worked on, and it is per interval
	// rather than per decision because it genuinely varies: a parent that names no
	// venue leaves each child to the router's own choice, so one decision can be
	// worked across two books. Measuring both against one book's candles would
	// divide by a denominator that never carried those fills.
	Venue    string
	From, To time.Time
	Filled   *big.Rat
}

// traded reports whether this interval contributed to the decision at all. A
// child that was admitted and never filled participated in nothing, and counting
// it would put a zero into a maximum that is supposed to find the worst slice.
func (in Interval) traded() bool {
	return in.Filled != nil && in.Filled.Sign() > 0 && in.To.After(in.From)
}

// Participation is one decision's realised participation, as far as it could be
// observed.
type Participation struct {
	// Overall is Σ filled over Σ realised volume, across the intervals that
	// traded AND could be seen. Nil when none could.
	//
	// THE UNOBSERVED INTERVALS LEAVE BOTH SUMS, not just the denominator.
	// Keeping their fills in the numerator while dropping their volume from the
	// denominator would inflate the rate by exactly the coverage gap — a
	// measurement that gets worse the less of the tape the platform can see, in
	// the direction that manufactures breaches.
	Overall *big.Rat

	// MaxSlice is the worst single observed interval, and MaxSliceIndex is which
	// one. Nil and -1 when nothing was observable.
	//
	// IT IS THE FIGURE A CAP ACTUALLY BOUNDS. POV promises "no child is ever
	// more than n% of its interval", which the aggregate above cannot express: a
	// parent that was 2% of a busy hour and 40% of one dead minute averages to a
	// comfortable number, and averaging a busy interval against a dead one is
	// precisely how an exceeded participation limit hides.
	MaxSlice      *big.Rat
	MaxSliceIndex int
	// MaxSliceVenue is the book that worst interval was worked on, so the
	// exceedance an operator is paged about names a book rather than a decision.
	MaxSliceVenue string

	Quality ParticipationQuality

	// Intervals is how many of the decision's slices actually traded; Measured
	// is how many of THOSE had realised volume behind them.
	//
	// BOTH ARE CARRIED EVEN THOUGH Quality ALREADY SAYS MEASURED OR PARTIAL,
	// for the reason Attribution carries Slices and Measured: "one interval of
	// fifty lost its candle" and "two of fifty were seen" are the same Quality
	// and two entirely different statements about whether the cap held.
	Intervals int
	Measured  int
}

// MeasureParticipation divides what a decision filled in each of its intervals
// by what actually printed in that interval.
//
// # IT RETURNS A VERDICT RATHER THAN AN ERROR
//
// Every way this can fail to produce a number — no intervals, no source, a
// source that cannot see the window — is an operational state a reader has to be
// able to tell apart, and an error collapses all of them into "no data". So the
// outcomes are values: NOT_WORKED, UNOBSERVABLE and PARTIAL each name a
// different thing to go and fix, and the caller counts them by name.
//
// A NIL SOURCE IS UNOBSERVABLE, NOT A PANIC AND NOT A ZERO. An OMS wired to no
// realised-volume feed measures nothing, and it must say so in the same
// vocabulary a live feed uses for a market it cannot see — the deployment gap is
// then visible in the quality counter rather than as a suspiciously calm rate.
func MeasureParticipation(instrumentID string, intervals []Interval, src RealisedVolume) Participation {
	out := Participation{MaxSliceIndex: -1, Quality: ParticipationNotWorked}

	traded := 0
	for _, in := range intervals {
		if in.traded() {
			traded++
		}
	}
	if traded == 0 {
		// NO INTERVAL TRADED. Either the decision was not worked as a schedule at
		// all, or every child was admitted and cancelled before it filled. Both
		// are "there is no participation to report", and neither is a coverage
		// gap the market-data edge should be asked about.
		return out
	}
	out.Intervals = traded
	out.Quality = ParticipationUnobservable

	filled := new(big.Rat)
	printed := new(big.Rat)
	for _, in := range intervals {
		if !in.traded() {
			continue
		}
		vol, known := volumeOf(src, instrumentID, in)
		if !known {
			continue
		}
		out.Measured++
		filled.Add(filled, in.Filled)
		printed.Add(printed, vol)

		rate := new(big.Rat).Quo(in.Filled, vol)
		if out.MaxSlice == nil || rate.Cmp(out.MaxSlice) > 0 {
			out.MaxSlice, out.MaxSliceIndex, out.MaxSliceVenue = rate, in.Index, in.Venue
		}
	}
	if out.Measured == 0 {
		return out
	}

	// printed IS POSITIVE BY CONSTRUCTION HERE — volumeOf refuses a non-positive
	// volume as unknown — so this division cannot be by zero. It is stated rather
	// than defended twice: the refusal is one line, in one place, and a second
	// guard here would be a second answer to "what counts as observable".
	out.Overall = new(big.Rat).Quo(filled, printed)
	if out.Measured == out.Intervals {
		out.Quality = ParticipationMeasured
	} else {
		out.Quality = ParticipationPartial
	}
	return out
}

// volumeOf asks the source about one interval and decides whether the answer can
// be divided by.
//
// A NON-POSITIVE VOLUME IS UNKNOWN, NOT A DENOMINATOR. A source that answers
// known=true with zero is claiming a minute in which the instrument traded
// nothing while this decision filled into it — which is either a feed that was
// not running or the most extreme participation event there is, and nothing here
// can tell those apart. Reporting it as an infinite rate would page on a dead
// feed; reporting it as zero would report the calmest possible participation in
// the one state the cap exists for. UNKNOWN is the only honest third answer, and
// it is counted as coverage rather than as a rate.
func volumeOf(src RealisedVolume, instrumentID string, in Interval) (*big.Rat, bool) {
	if src == nil {
		return nil, false
	}
	vol, known := src.Volume(instrumentID, in.Venue, in.From, in.To)
	if !known || vol == nil || vol.Sign() <= 0 {
		return nil, false
	}
	return vol, true
}

// Exceeds reports whether a measured participation breached the cap the decision
// was working under.
//
// IT ASKS THE MAXIMUM, NEVER THE AGGREGATE, because the cap is a per-interval
// promise: "no child is ever more than n% of its interval". A decision whose
// overall rate is 3% against an 8% cap and whose worst minute was 40% broke the
// control, and the aggregate says it did not.
//
// A cap that is absent or outside (0, 1] is not a cap — pov.go refuses one at
// admission for the same reason — so there is nothing to exceed and this is
// false. So is an unobservable measurement: the whole point of the quality
// vocabulary is that "not contradicted" and "confirmed" are different, and a
// counter incremented on the first would be a breach nobody can point at.
func (p Participation) Exceeds(capRate *big.Rat) bool {
	if capRate == nil || capRate.Sign() <= 0 {
		return false
	}
	if p.MaxSlice == nil {
		return false
	}
	return p.MaxSlice.Cmp(capRate) > 0
}
