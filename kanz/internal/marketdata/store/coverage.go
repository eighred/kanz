package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// THE INGESTION-COVERAGE RECORD, store side (#591, #416).
//
// gaps.go states the property that made this necessary:
//
//	missing bucket  ⇒  we cannot assert what happened in it.        SOUND.
//	whole window    ⇒  the platform was up and observing.           NOT SOUND.
//
// and names what would make the second direction sound: "an INGESTION-COVERAGE
// RECORD stating which intervals were actually OBSERVED". This is where that
// record lands.
//
// # IT IS NOT DERIVED FROM THE BARS, AND MUST NEVER BE
//
// Every row here was written by the process that HELD THE VENUE SUBSCRIPTION,
// about that subscription, and published as a FACT
// (internal/marketedge/coverage). Nothing in this package computes a Coverage
// from a Bar, and nothing downstream may: a record derived from the series it
// vouches for vouches for itself, and would turn a silent outage into a
// confident "the market was quiet".
//
// # IT ONLY EVER EXISTS GOING FORWARD
//
// Nothing can reconstruct whether a feed was live last July. Intervals older
// than the attestor read as UNKNOWN and must stay that way — there is no
// backfill for this table and there must never be one. Coverage.Validate
// enforces the parts of that it can; the rest is the reader's discipline, which
// is why Attested below reports Unknown as its own number rather than folding it
// into a boolean.

// ErrInvalidCoverage is returned by Coverage.Validate.
var ErrInvalidCoverage = errors.New("store: invalid coverage record")

// Coverage is one attestation: how much of one interval one feed subscription
// was demonstrably live for one (instrument, venue).
type Coverage struct {
	// InstrumentID and Venue identify the subscription's subject. VENUE IS PART
	// OF THE IDENTITY, exactly as it is for Bar: a live Binance subscription
	// says nothing whatever about what OKX was doing.
	InstrumentID string
	Venue        string
	// Resolution is the interval this row attests, and it matches the bar series
	// it explains. A coverage bucket that did not line up with a bar bucket would
	// answer a question nobody asked.
	Resolution Resolution
	// BucketStart is the inclusive start of the attested interval.
	BucketStart time.Time
	// Observed is how much of [BucketStart, BucketStart+interval) the
	// subscription was proven live for.
	//
	// A LOWER BOUND, NEVER AN ESTIMATE. Time the attestor could not prove is not
	// credited, so Observed short of the interval means "cannot vouch for the
	// difference" — never "the feed was definitely down for it". The direction
	// that must never fail is the other one: Observed is never longer than was
	// actually proven.
	Observed time.Duration
	// Breaks is how many times the subscription was observed to FAIL inside the
	// interval. Zero with a short Observed means the attestor simply stopped
	// hearing from the socket; non-zero names a fault that was actually seen.
	Breaks int32
	// Attestor names the subscription that made the claim, so a coverage record
	// can be traced to the thing that produced it rather than to a service name.
	// It is part of the key: two subscriptions may cover one series, and a row
	// whose origin is unknown cannot be audited.
	Attestor string
	// RecordedAt is when Kanz learned this attestation.
	RecordedAt time.Time
}

// Validate refuses a record that cannot be read as an attestation.
//
// THE OVER-CLAIM IS THE ONE THAT MATTERS. Observed longer than the interval is
// refused rather than clamped: a clamp would silently turn an attestor bug into
// the strongest possible claim about a window, which is exactly the direction
// this record must never fail in.
func (c Coverage) Validate() error {
	if c.InstrumentID == "" {
		return fmt.Errorf("%w: empty instrument_id", ErrInvalidCoverage)
	}
	if c.Venue == "" {
		return fmt.Errorf("%w: empty venue — coverage is per venue, and a record that did not "+
			"say which feed it attests vouches for a subscription nobody held", ErrInvalidCoverage)
	}
	interval, ok := c.Resolution.Interval()
	if !ok || interval <= 0 {
		return fmt.Errorf("%w: resolution %q is not one this platform stores", ErrInvalidCoverage, c.Resolution)
	}
	if c.BucketStart.IsZero() {
		return fmt.Errorf("%w: zero bucket_start", ErrInvalidCoverage)
	}
	if c.Attestor == "" {
		return fmt.Errorf("%w: empty attestor — a coverage claim nothing can be traced to "+
			"cannot be audited, and an unauditable claim is worse than no claim", ErrInvalidCoverage)
	}
	if c.Observed < 0 {
		return fmt.Errorf("%w: negative observed duration %s", ErrInvalidCoverage, c.Observed)
	}
	if c.Observed > interval {
		return fmt.Errorf("%w: observed %s exceeds the %s interval it attests — an attestor "+
			"cannot have proven more time than the bucket holds, and clamping it here would turn "+
			"the bug into a maximally confident claim", ErrInvalidCoverage, c.Observed, interval)
	}
	if c.Breaks < 0 {
		return fmt.Errorf("%w: negative break count %d", ErrInvalidCoverage, c.Breaks)
	}
	if c.RecordedAt.IsZero() {
		return fmt.Errorf("%w: zero recorded_at — see ingest.go's knowledgeTime: an unstamped "+
			"envelope is refused, not substituted", ErrInvalidCoverage)
	}
	return nil
}

// Whole reports that the subscription was proven live for the ENTIRE interval.
//
// This is the one claim the bar series cannot make about itself, and the only
// thing that turns a missing bar into a QUIET market rather than an UNKNOWN one.
//
// It is NOT "we saw every print": a venue can drop a message on a healthy socket
// and no attestor would know. It is "we were looking, for all of it".
func (c Coverage) Whole() bool {
	interval, ok := c.Resolution.Interval()
	return ok && interval > 0 && c.Observed >= interval
}

// CoverageQuery selects a coverage series. Empty From/To are unbounded.
type CoverageQuery struct {
	InstrumentID string
	Venue        string
	Resolution   Resolution
	// From is inclusive on BucketStart, To exclusive — the same bounds BarQuery
	// uses, so a window queried from both stores means one thing.
	From time.Time
	To   time.Time
}

// CoverageStore is the ingestion-coverage record's durable surface.
//
// IT IS A SEPARATE INTERFACE FROM BarStore ON PURPOSE. The two must be
// independently substitutable in a test, because the whole value of this record
// is that it disagrees with the bar series: a fake that produced coverage from
// whatever bars it held would make every test pass and prove nothing.
type CoverageStore interface {
	// PutCoverage stores attestations idempotently.
	//
	// THE MERGE IS max(Observed), NOT LAST-WRITE-WINS, and it is the contract
	// rather than an implementation detail. Two processes may hold independent
	// subscriptions to one series; each attests only what IT proved. The union is
	// at least the larger of them, so taking the maximum is a sound lower bound
	// while overwriting with a later, shorter claim would silently retract
	// coverage that was genuinely observed.
	PutCoverage(ctx context.Context, cov []Coverage) error
	// Coverage returns the attestations for one series, ascending by
	// BucketStart. An interval with no row is UNKNOWN — never "not covered", and
	// never "quiet".
	Coverage(ctx context.Context, q CoverageQuery) ([]Coverage, error)
}

// validateCoverageQuery is shared by both implementations so the memory store
// cannot accept a query Postgres refuses.
func validateCoverageQuery(q CoverageQuery) error {
	if q.InstrumentID == "" {
		return errors.New("store: coverage with empty instrument_id")
	}
	if q.Venue == "" {
		return errors.New("store: coverage with empty venue — coverage is per venue, and " +
			"collapsing venues would vouch for a feed nobody was holding")
	}
	if !q.Resolution.Valid() {
		return fmt.Errorf("store: coverage with resolution %q, which is not one this platform stores",
			q.Resolution)
	}
	return nil
}
