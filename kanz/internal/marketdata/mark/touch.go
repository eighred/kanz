package mark

import (
	"math/big"
	"time"
)

// THE TOUCH: THE MARKET'S WIDTH, KEPT BESIDE THE MARK (#866).
//
// This fold already reads every Quote on the spine and already computes a mid
// from bid and ask — and then throws both legs away, because a mid was all any
// caller needed. It is not enough for execution measurement: implementation
// shortfall decomposes into what it cost to CROSS the quoted market (the
// half-spread), what the market did on its own while the order was worked, and
// what is left. The first of those three is not derivable from a mid at all, at
// any later time, by anybody.
//
// So the two legs are retained here rather than recomputed anywhere else. The
// alternative — a second fold of the same subjects, in the OMS, to hold bid and
// ask — is the shape AGENTS.md names: one concept, two implementations, and the
// first divergence is whichever of the two nobody remembers to fix. A leverage
// cap checked against one feed and a spread measured against another would give
// two answers about the same instant for reasons no operator could see.
//
// # Why a SEPARATE map rather than two fields on entry
//
// Because a Trade and a Quote are different observations with different times,
// and the mark takes whichever arrived last. Widening `entry` would mean a trade
// print either erasing a perfectly good touch (a mark-bearing trade is common on
// a thin book) or inheriting the quote's bid/ask under the TRADE's asOf — a
// touch stamped with a time it was not observed at, which is the kind of quiet
// wrongness that only shows up as a cost number nobody can explain.
//
// Two maps means two observation times, each true about its own event, and each
// expiring on its own. The cost is one extra map entry per QUOTED instrument,
// bounded by the same sweep the price map is.
type touch struct {
	bid  *big.Rat
	ask  *big.Rat
	asOf time.Time
}

// Touch returns the latest non-expired quoted market for an instrument.
//
// IT IS THE SAFE ACCESSOR, with the same contract Mark has: ok == false when
// the instrument was never quoted or when the newest quote is older than
// touchMaxAge — the width's OWN bound, which is shorter than the mark's
// wherever a caller set one (#956) — and the caller's job is then to record
// NOTHING rather than a zero
// width. A zero spread is a real and different claim from an unobservable one —
// it says the book was tight and crossing was free — and an execution report
// that cannot tell them apart will score every unquoted instrument as the
// cheapest thing the fund trades.
//
// A CROSSED OR INVERTED QUOTE IS REFUSED (ask <= bid). It is not a tight market,
// it is a book this fold cannot have seen in one consistent state — two venues'
// legs blended, or a stale side — and a negative half-spread would arrive in the
// attribution as a NEGATIVE cost of crossing, which reads as the fund being paid
// to take liquidity.
//
// The returned values are copies; the caller may mutate them.
func (s *Source) Touch(instrument string) (bid, ask *big.Rat, asOf time.Time, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, seen := s.touches[instrument]
	if !seen || !s.touchUsableLocked(t) {
		return nil, nil, time.Time{}, false
	}
	return new(big.Rat).Set(t.bid), new(big.Rat).Set(t.ask), t.asOf, true
}

// touchUsableLocked is the ONE definition of "this fold can answer for that
// instrument right now". The caller holds at least the read lock.
//
// IT IS SHARED WITH TouchStats DELIBERATELY, and that is the whole reason it
// exists as a named predicate rather than two inline conditions. The gauge below
// is read by an alert whose entire claim is "Touch would refuse for everything"
// — so a gauge computed from a SECOND, separately-written expiry test is a
// coverage number that can disagree with the coverage it reports on. The
// divergence would not look like a bug: it would look like an execution-quality
// problem on a healthy spine, or silence on a dead one, and the metric is the
// only place either could be seen from.
// IT READS touchMaxAge AND NOT maxAge (#956). The mark's bound is six missed
// ticker polls; the same number against a 1s quote cadence is thirty missed
// publishes, and the width is the leg a spread cost is computed from. The two
// producers are different, so the two bounds are.
func (s *Source) touchUsableLocked(t touch) bool {
	if t.bid == nil || t.ask == nil {
		return false
	}
	return !(s.touchMaxAge > 0 && s.now().Sub(t.asOf) > s.touchMaxAge)
}

// recordTouchLocked stores one quote's two legs. The caller holds the write lock
// and has already validated both prices through the same domain check the mid
// went through, so nothing here can be reached with an out-of-domain exponent.
//
// A CROSSED QUOTE IS DROPPED AT THE FOLD as well as refused at the read. Storing
// it would let Touch's guard be the only thing standing between an inverted book
// and a negative spread cost, and this fold's own rule everywhere else is that
// an unusable observation is never written down.
func (s *Source) recordTouchLocked(instrument string, bid, ask *big.Rat, asOf time.Time) {
	if bid == nil || ask == nil || bid.Sign() <= 0 || ask.Sign() <= 0 || ask.Cmp(bid) <= 0 {
		return
	}
	s.touches[instrument] = touch{bid: new(big.Rat).Set(bid), ask: new(big.Rat).Set(ask), asOf: asOf}
}

// sweepTouchesLocked drops every expired quote outright, rather than tombstoning
// it the way an expired price is.
//
// THE ASYMMETRY IS DELIBERATE AND IT IS NOT AN OVERSIGHT. A price tombstone
// exists so a caller can tell "never seen" from "seen but stalled" — the OMS
// branches on exactly that to emit two different operator messages (#96). Touch
// has no such caller and no such branch: an instrument with no observable width
// is unmeasurable for the same reason whether the spine never quoted it or
// stopped, the attribution records nothing either way, and the price map's
// tombstone already carries the staleness diagnosis for that instrument. Keeping
// a second tombstone population would be memory held to distinguish two cases
// nothing distinguishes.
//
// IT SWEEPS ON touchMaxAge, the same bound touchUsableLocked reads, so an
// entry Touch refuses is an entry the next sweep drops. Reading the mark's
// maxAge here instead would hold every width for the longer of the two bounds
// — memory retained for entries no accessor can answer from — and would make
// TouchStats's held count a population with no reader.
func (s *Source) sweepTouchesLocked(now time.Time) {
	if s.touchMaxAge <= 0 {
		return
	}
	for id, t := range s.touches {
		if now.Sub(t.asOf) > s.touchMaxAge {
			delete(s.touches, id)
		}
	}
}

// TouchStats reports how many instruments this fold holds a quoted width for,
// and how many of those Touch would actually answer for. It is the pair Stats is
// for prices, and it is the coverage signal an execution-quality report needs: a
// decomposition that keeps coming back TOTAL_ONLY is a QUOTE-spine gap, and this
// is what says how wide it is — before an order is measured, rather than
// inferred afterwards from a cost report (#875).
//
// HELD ALONE WOULD HAVE OVERSTATED IT, which is why this returns two numbers and
// not the one it returned when it had no caller. `len(touches)` counts entries
// the sweep has not reached yet: sweepTouchesLocked is throttled to sweepInterval
// and runs only inside a fold, so a spine that stops quoting leaves every width
// in the map — expired, unusable, and indistinguishable from coverage — until
// the next Quote arrives to trigger the sweep. On a spine that stops quoting
// ENTIRELY, which is the exact condition this measures, that next Quote never
// comes and held stays at its high-water mark forever. A gauge reading full
// coverage on a dead quote feed is the "nothing configured" / "checked, and
// fine" failure with a number attached.
//
// live is therefore computed through touchUsableLocked — the same predicate
// Touch reads — rather than from the map's cardinality. held is kept beside it
// because held − live is the expired-width population, which distinguishes a
// spine that never quoted these instruments (both zero) from one that quoted and
// stopped (held high, live zero) exactly as Stats does for marks.
func (s *Source) TouchStats() (held, live int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.touches {
		if s.touchUsableLocked(t) {
			live++
		}
	}
	return len(s.touches), live
}
