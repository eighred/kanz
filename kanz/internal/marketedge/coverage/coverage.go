// Package coverage is the INGESTION-COVERAGE RECORD: a first-hand statement of
// which intervals this platform was actually OBSERVING (#591, #416).
//
// # The hole it fills
//
// internal/marketedge/bars/fold.go emits NO BAR for a minute in which nothing
// traded, and says why it cannot do better:
//
//	this process cannot tell "nothing traded" from "the feed was down" or "we
//	were rolling pods" — they look identical from here.
//
// internal/marketdata/rollup/driver.go ruled on what that costs downstream, and
// named the missing piece by name:
//
//	The fix does not belong here: it belongs to an ingestion-coverage record that
//	states which intervals were actually OBSERVED, which this platform does not
//	yet have.
//
// internal/marketdata/store/gaps.go carries the property that decides this
// package's entire design:
//
//	missing bucket  ⇒  we cannot assert what happened in it.        SOUND.
//	whole window    ⇒  the platform was up and observing.           NOT SOUND.
//
// This is what makes the second direction provable. Everything here exists to
// keep it sound.
//
// # WHY IT IS RECORDED HERE AND NOT DOWNSTREAM
//
// The only component that KNOWS whether a feed was live is the one holding the
// subscription. Anything further down sees the same absence the fold sees and can
// only guess — and a record derived from the bars would be vouching for the data
// it exists to vouch FOR, which is circular and worth nothing. So the attestation
// is made at the socket, by the socket, and published as a FACT.
//
// For the same reason nothing in this package reads a price, a size or a volume.
// A coverage record that counted trades would invite exactly the reconciliation
// it is built to make unnecessary.
//
// # WHAT IT ATTESTS, STATED NARROWLY
//
// "For this (instrument, venue) and this minute, a subscription was established
// and round-tripping frames for D of the 60 seconds." Not "the venue was
// healthy", not "we saw every print" — the venue can drop a message on a live
// socket and nothing here would know. D is a LOWER BOUND, and the direction of
// the error is the whole point: uncredited time reads as "cannot vouch", never as
// "the feed was down", and credited time is never longer than was proven.
//
// # COVERAGE ONLY EXISTS GOING FORWARD
//
// Nothing can reconstruct whether a feed was live last July. There is no backfill
// for this record and there must never be one: a fabricated attestation is worse
// than none, because none is honestly UNKNOWN while a fabricated one is a lie a
// calibration report will price in. Intervals before this package was deployed
// have no record and must read as UNKNOWN forever.
package coverage

import (
	"errors"
	"fmt"
	"time"
)

// Resolution is the interval one coverage record attests.
//
// IT MUST EQUAL bars.Resolution. The record's only use is to explain an absence
// in the 1-minute bar series, so a coverage bucket that does not line up with a
// bar bucket answers a question nobody asked. TestResolutionMatchesBars asserts
// the equality rather than leaving it to a comment; this package does not import
// bars, because coverage must never depend on the series it vouches for.
const Resolution = time.Minute

// Series identifies one subscription's subject: one instrument on one venue.
//
// COVERAGE IS PER VENUE. A live Binance subscription says nothing whatever about
// what OKX was doing, and merging them would produce a record that vouches for a
// feed nobody was holding.
type Series struct {
	InstrumentID string
	Venue        string
}

func (s Series) String() string { return s.InstrumentID + "@" + s.Venue }

// Record is one completed attestation, ready to publish.
type Record struct {
	Series      Series
	BucketStart time.Time
	// Observed is how much of [BucketStart, BucketStart+Resolution) the
	// subscription was proven live for. A LOWER BOUND — see the package note.
	Observed time.Duration
	// Breaks is how many times the subscription was observed to FAIL inside the
	// bucket. Zero with a short Observed means the attestor simply stopped
	// hearing from the socket; non-zero names a fault that was actually seen.
	Breaks uint32
	// Attestor names the subscription that is speaking, so a claim can be traced
	// to the thing that made it.
	Attestor string
}

// Whole reports that the subscription was proven live for the entire bucket.
//
// READ IT AS "we were looking for all of it", which is exactly the claim the bar
// series cannot make about itself. It is NOT "we saw every print": a venue can
// drop a message on a healthy socket and nothing here would know.
func (r Record) Whole() bool { return r.Observed >= Resolution }

// ErrSilenceTolerance is returned when MaxSilence is unusable.
var ErrSilenceTolerance = errors.New("coverage: silence tolerance")

// state is one series' in-progress accounting. Not safe for concurrent use; the
// Recorder owns the lock.
type state struct {
	series   Series
	attestor string

	// open is false until the first observation. NOTHING IS ATTESTED BEFORE THE
	// FIRST ONE — a series this process never heard from produces no record at
	// all, which reads downstream as UNKNOWN. That is the honest answer and it is
	// deliberately distinct from a record saying zero.
	open bool
	// bucket is the start of the interval currently being accumulated.
	bucket time.Time
	// cursor is the instant up to which this bucket has been decided. Time before
	// it is settled; time after it has not been looked at yet.
	cursor time.Time
	// observed is the credited live time inside bucket.
	observed time.Duration
	breaks   uint32
	// last is the newest liveness observation, and live says whether the
	// subscription is believed live as of it. A Down clears live so the next
	// observation cannot credit the outage it spans.
	last time.Time
	live bool
}

// advance settles the interval [cursor, to], emitting every bucket that ends on
// the way, and crediting the traversed time as observed only when credit is true.
//
// EVERY UNCREDITED SECOND IS A SECOND NOBODY VOUCHES FOR. That is the direction
// the record must fail in: under-crediting produces a window that refuses to
// support a claim, over-crediting produces one that supports a false claim.
func (s *state) advance(to time.Time, credit bool, emit func(Record)) {
	if to.Before(s.cursor) {
		// Out of order. A clock that went backwards must not un-settle time that
		// has already been decided, and must never credit it twice.
		return
	}
	for {
		end := s.bucket.Add(Resolution)
		if to.Before(end) {
			if credit {
				s.observed += to.Sub(s.cursor)
			}
			s.cursor = to
			return
		}
		if credit {
			s.observed += end.Sub(s.cursor)
		}
		emit(Record{
			Series:      s.series,
			BucketStart: s.bucket,
			Observed:    s.observed,
			Breaks:      s.breaks,
			Attestor:    s.attestor,
		})
		s.bucket = end
		s.cursor = end
		s.observed = 0
		s.breaks = 0
		if to.Equal(end) {
			return
		}
	}
}

// validateSilence enforces the two bounds MaxSilence must satisfy.
func validateSilence(d time.Duration) error {
	if d <= 0 {
		// A zero here would be a default that looks healthy: with no tolerance
		// every observation lands after an "excessive" silence, every second is
		// uncredited, and the record silently reports that the platform observed
		// nothing while the feed runs perfectly.
		return fmt.Errorf("%w: must be positive — there is no safe default, it is a "+
			"property of the attesting feed's heartbeat cadence", ErrSilenceTolerance)
	}
	if d >= Resolution {
		// A tolerance at or beyond one bucket lets a whole silent bucket be
		// credited as live off a single observation on either side of it, which
		// is precisely the claim this record exists to refuse.
		return fmt.Errorf("%w: %s is not shorter than the %s bucket — a tolerance that "+
			"spans a whole interval would credit a silent one as observed",
			ErrSilenceTolerance, d, Resolution)
	}
	return nil
}
