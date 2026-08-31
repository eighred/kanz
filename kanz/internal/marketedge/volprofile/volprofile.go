// Package volprofile is the INTRADAY VOLUME PROFILE: how a session's traded
// volume distributes across the day, per instrument PER VENUE (#867).
//
// # The hole it fills
//
// internal/marketedge already holds an L2 book, a retention-bounded trade tape
// and a one-minute bar fold. None of them answers "when does this instrument
// trade?", so a scheduler has nothing to slice against but the wall clock. An
// equal-slice schedule meets the last hour of a session — which on a normal day
// carries a disproportionate share of the prints — with the same size it used at
// midday. The order pays spread it did not have to pay, and signals itself in the
// thin periods.
//
// This package is the datum. The algorithms that consume it are arithmetic on top
// of it and live elsewhere; nothing here schedules anything.
//
// # THE PROPERTY THAT DECIDES THE WHOLE DESIGN: A FLAT PROFILE IS TWAP
//
// The tempting failure is to answer a series nobody has measured with equal
// shares. It is well-formed, it is in range, and every arithmetic built on it
// works. It is also, exactly, TWAP — so a VWAP order scheduled from it becomes a
// TWAP order wearing the wrong name, and the execution attribution that later
// asks which algorithm ran would name one that did not.
//
// So the answer to "what is this instrument's shape?" has three states and not
// two, in the estate's usual sense: a measured shape, or UNKNOWN with a reason,
// and never a default that looks healthy. Answer.Shares is nil unless
// Verdict.Known, and VerdictAbsent is the ZERO VALUE of Verdict so that an answer
// nobody filled in fails closed.
//
// # WHY THE PROFILE IS PER VENUE
//
// The same reason internal/marketedge/coverage attests per venue and
// internal/marketedge/bars keys candles by one: two venues on one instrument have
// different intraday shapes — different participants, different funding and
// settlement cycles, different maintenance windows. A profile keyed only by
// instrument is a wrong number rather than a simplification, because an order
// executes against ONE book and would be scheduled against a shape that is a
// blend of books it will never touch.
//
// # WHAT THIS PACKAGE CANNOT SAY, STATED SO THE COUNTS ARE NOT OVERREAD
//
// internal/marketedge/bars/fold.go's rule applies here unchanged: a bucket in
// which nothing traded is indistinguishable from one in which the feed was down.
// The estate's answer to that is the ingestion-coverage attestation in
// internal/marketedge/coverage, and it does not reach this package — coverage is
// published as a FACT and consumed downstream, while this fold sits on the raw
// feed beside the tape.
//
// So this package borrows the vocabulary and refuses the claim. Window counts
// Buckets and Observed, and everything else is Unknown. There is deliberately NO
// Quiet counter, because a Quiet count here would be a measured-absence claim
// nobody made — the exact conflation store.Attested exists to break, and the one
// it can break only because it holds a first-hand attestation.
//
// Unknown is REPORTED AND NEVER GATED, which is the asymmetry
// internal/alpha/barview already settled: a shape computed over the buckets that
// ARE there is provable from what was seen, and refusing every session with a
// hole would blank the profile on every instrument thin enough to skip a bucket,
// which is most of them. A scheduler decides with the counter in hand.
package volprofile

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eighred/kanz/internal/marketedge/trades"
)

// Session is the profile's day.
//
// TWENTY-FOUR HOURS FROM UTC MIDNIGHT, because the venues this edge subscribes to
// are crypto venues and they do not close. There is no open, no auction and no
// half-day to model, so the session boundary is the calendar boundary the rest of
// the platform already timestamps against. time.Time.Truncate is measured from a
// UTC midnight, so truncating to this constant lands exactly on it.
//
// A venue with a real trading calendar cannot be profiled by truncation and must
// not be forced through this constant: its session boundaries are reference data,
// and folding a 6.5-hour session into a 24-hour bin set would put 71% of every
// day into buckets that cannot trade. That is a refusal to be made when such a
// venue is onboarded, not a default to be quietly stretched.
const Session = 24 * time.Hour

// DefaultBucket is the intraday bin width.
//
// THIRTY MINUTES, and the number comes from the shape it has to resolve. The
// feature that motivates the whole profile is a concentration of volume in the
// closing stretch of a session — an effect measured in HOURS. A bin must be
// several times finer than the feature it resolves or it cannot see it, and at
// 30 minutes the final hour is two bins rather than one step.
//
// FINER DOES NOT BUY SHAPE. The share in each bin is estimated from one sample
// per retained session, so halving the bin halves the volume behind each estimate
// while leaving the number of samples alone: at five-minute bins the profile's
// own sampling noise exceeds the intraday structure it is meant to describe, and
// a scheduler would chase it. Thirty minutes is also a whole number of
// bars.Resolution one-minute candles and divides the session evenly into 48, so a
// bucket is a whole number of the platform's base series in both directions —
// New refuses a width for which either is untrue.
const DefaultBucket = 30 * time.Minute

// DefaultHorizon is how far back completed sessions are retained.
//
// THREE WEEKS, AND THE WHOLE-WEEK PART IS THE ARGUMENT. An intraday shape has a
// day-of-week component — crypto weekends are materially thinner than weekdays —
// so a horizon that is not a whole number of weeks samples some weekdays more
// often than others purely by where the window happens to end, and the resulting
// shape carries that sampling artefact as if it were market structure. A whole
// number of weeks gives every weekday the same number of samples.
//
// THREE RATHER THAN ONE is the shortest whole-week window in which one anomalous
// session — a liquidation cascade, a venue outage, a listing — no longer decides
// its weekday, because that weekday has two other samples to be averaged against.
//
// LIKE THE POINT-IN-TIME PRICING STORES, THIS BOUNDS AN ALREADY-BOUNDED THING.
// The store is in-memory and starts empty; nothing persists or replays it, so a
// pod restart leaves it holding no sessions at all and the depth a read can reach
// is already capped by process uptime. The horizon is what stops a long-lived
// market-ingest pod from accumulating sessions for the life of the deployment.
const DefaultHorizon = 21 * Session

// ErrConfig is returned by New for a configuration that cannot be made safe.
var ErrConfig = errors.New("volprofile: configuration")

// ErrNoAsOf is returned by Profile for a zero as-of.
//
// THERE IS NO DEFAULT TO NOW. A default would be correct for a live scheduler and
// total look-ahead for a backtest — every read answering from today's knowledge
// over history — and it would arrive wearing the shape of a convenience. This is
// the refusal internal/alpha/barview already makes for the same reason.
var ErrNoAsOf = errors.New("volprofile: as-of is required and does not default to now")

// ErrLookahead is returned by Profile when the as-of precedes the end of the
// newest completed session retained.
//
// The store only ever moves forward, so such a read would be answered with a
// shape containing sessions the caller could not have seen. It is the one leak
// that flatters and shows least: the shares are real, from real sessions, and
// describe a future the order did not have.
var ErrLookahead = errors.New("volprofile: as-of precedes the newest completed session")

// Series identifies one profile's subject: one instrument on one venue.
//
// It is the same two fields as bars.Series and coverage.Series, and it is a third
// copy rather than an import because both of those packages carry a bus publisher
// this one never uses — taking either would put protobuf and the broker client
// into the dependency set of a package that folds arithmetic. The agreement is
// asserted by TestSeriesMatchesTheBarSeries rather than left to this paragraph,
// exactly as coverage asserts its resolution against the bar fold's.
type Series struct {
	InstrumentID string
	Venue        string
}

func (s Series) String() string { return s.InstrumentID + "@" + s.Venue }

// Verdict is what a profile query concluded.
type Verdict uint8

const (
	// VerdictAbsent is the ZERO VALUE, deliberately: an Answer nobody filled in
	// reads as "no profile exists", which is the direction that fails closed. A
	// known answer has to be constructed on purpose.
	VerdictAbsent Verdict = iota
	// VerdictStale means the newest completed session is further back than the
	// horizon — the profile describes a market that has since moved on.
	VerdictStale
	// VerdictTooFewSessions means the store holds less history than the caller
	// said it requires before it would schedule against a shape.
	VerdictTooFewSessions
	// VerdictKnown means Shares carries a measured distribution.
	VerdictKnown
)

// Known reports whether the answer carries a measured shape.
func (v Verdict) Known() bool { return v == VerdictKnown }

func (v Verdict) String() string {
	switch v {
	case VerdictAbsent:
		return "ABSENT"
	case VerdictStale:
		return "STALE"
	case VerdictTooFewSessions:
		return "TOO_FEW_SESSIONS"
	case VerdictKnown:
		return "KNOWN"
	default:
		return fmt.Sprintf("Verdict(%d)", uint8(v))
	}
}

// Window is how much of the retained history was actually observed.
//
// IT IS store.Attested's VOCABULARY WITHOUT store.Attested's QUIET COUNTER, and
// the missing counter is the honest part. That type can call a missing bucket
// Quiet because it holds a first-hand ingestion attestation; this package holds
// none, so every unobserved bucket is Unknown. Reporting some of them as Quiet
// would be a measured-absence claim nobody made.
type Window struct {
	// Buckets is how many (session, bucket) pairs the retained history spans.
	Buckets int
	// Observed is how many of them saw at least one print.
	Observed int
}

// Unknown is how many buckets nobody can speak for.
func (w Window) Unknown() int {
	if w.Observed >= w.Buckets {
		return 0
	}
	return w.Buckets - w.Observed
}

// Sound reports that every bucket in the retained history was observed.
//
// READ IT AS "nothing in this shape is a guess". A window of zero buckets is
// Sound vacuously, exactly as store.Window is Whole — a caller gating on this
// decides what an empty window means for it rather than inheriting an answer.
func (w Window) Sound() bool { return w.Unknown() == 0 }

// Answer is one profile query's result.
//
// EXACTLY ONE THING MAKES IT USABLE: Verdict.Known. Shares is nil in every other
// state, so a caller that reads it without checking gets a nil slice rather than
// a flat shape — the failure is loud instead of a schedule that looks right.
type Answer struct {
	Series Series
	// Bucket is the width each entry of Shares covers.
	Bucket time.Duration
	// AsOf is the instant the query was answered as of.
	AsOf time.Time
	// Verdict says whether a shape could be asserted, and why not when it could
	// not.
	Verdict Verdict
	// Sessions is how many completed sessions the store retains for this series.
	// Reported in EVERY state, including the refusals: an operator has to be able
	// to tell "nearly ready" from "nothing here".
	Sessions int
	// Oldest and Newest are the first and last retained completed sessions'
	// starts. Zero when none are retained.
	Oldest, Newest time.Time
	// Window is the observation accounting over those sessions.
	Window Window
	// Shares is the mean of each retained session's own distribution, one entry
	// per intraday bucket, summing to exactly 1. NIL UNLESS Verdict.Known.
	Shares []*big.Rat
}

// Known reports whether this answer carries a measured shape.
func (a Answer) Known() bool { return a.Verdict.Known() }

// Share returns the expected share of a session's volume in the bucket
// containing `at`, and ok=false when this answer carries no shape.
//
// The returned value is the caller's own; mutating it cannot reach the store.
func (a Answer) Share(at time.Time) (*big.Rat, bool) {
	if !a.Known() || len(a.Shares) == 0 || a.Bucket <= 0 {
		return nil, false
	}
	at = at.UTC()
	i := int(at.Sub(at.Truncate(Session)) / a.Bucket)
	if i < 0 || i >= len(a.Shares) {
		return nil, false
	}
	return new(big.Rat).Set(a.Shares[i]), true
}

// BucketsPerSession is how many bins of the given width a session divides into.
// Zero for a width that does not divide it.
func BucketsPerSession(bucket time.Duration) int {
	if bucket <= 0 || Session%bucket != 0 {
		return 0
	}
	return int(Session / bucket)
}

// Config configures a Store.
type Config struct {
	// Bucket is the intraday bin width. Zero uses DefaultBucket. It must divide
	// the session evenly AND be a whole number of minutes.
	Bucket time.Duration
	// Horizon is how far back completed sessions are retained, measured from the
	// newest one retained. Zero uses DefaultHorizon; anything positive and shorter
	// than one session is refused.
	Horizon time.Duration
	// MinSessions is how many completed sessions must be retained before a shape
	// may be asserted.
	//
	// REQUIRED, AND THERE IS NO DEFAULT. How much history a desk demands before it
	// will schedule against a shape is an execution-policy decision, not a
	// property of the market data — a number invented here would either refuse a
	// healthy profile or let a two-day sample set a month's schedule, and
	// "nothing configured" would look exactly like "checked, and fine". This is
	// the stance coverage.Config takes for its silence tolerance.
	MinSessions int
}

// session is one day's accumulation for one series.
//
// A POINTER TYPE, so the retention test can attach a finalizer to the element a
// prune is about to drop and state release as a fact about the heap rather than
// about a slice header.
type session struct {
	start time.Time
	// vol[i] is the exact volume printed in bucket i. Never nil.
	vol []*big.Rat
	// seen[i] records that bucket i saw at least one print. A zero in vol with
	// seen false is an interval nobody can speak for; with seen true it would be
	// a measured zero, which cannot occur because a print is what sets seen.
	seen  []bool
	total *big.Rat
	// prints is how many trades folded into this session (observability).
	prints int64
}

func newSession(start time.Time, buckets int) *session {
	s := &session{
		start: start,
		vol:   make([]*big.Rat, buckets),
		seen:  make([]bool, buckets),
		total: new(big.Rat),
	}
	for i := range s.vol {
		s.vol[i] = new(big.Rat)
	}
	return s
}

// history is one series' retained profile state.
type history struct {
	// open is the session in progress. It does NOT contribute to the shape: a
	// partial session is the hours that have already elapsed, and folding it in
	// tilts the shape toward the morning every morning.
	open *session
	// done is the retained completed sessions, ascending by start.
	done []*session
	// shape is the cached distribution, and dirty is what invalidates it. THE
	// PROFILE IS BUILT ONCE AND READ MANY TIMES — recomputing per order would put
	// one exact division per retained bucket per session onto the order path.
	shape []*big.Rat
	dirty bool
	// window is the observation accounting computed alongside shape.
	window Window
}

// Store folds live trades into per-series intraday volume profiles.
//
// Safe for concurrent use: each feed goroutine calls Observe for its own series
// while readers call Profile, exactly as trades.Tape is appended to by a fold
// goroutine while engine ticks read it.
type Store struct {
	mu sync.RWMutex
	// series is keyed by (instrument, venue) and is bounded by the subscriptions
	// this process holds — one entry per feed it has ever folded, not one per
	// order or per message. It is a cardinality-bounded map like
	// livequote.LiveQuotes, not a monotonic one, and it needs no evictor for the
	// same reason.
	series map[Series]*history

	bucket      time.Duration
	buckets     int
	horizon     time.Duration
	minSessions int

	// recomputes counts shape rebuilds, so a test can state that a read does not
	// cause one. late counts prints for a session already folded.
	recomputes atomic.Int64
	late       atomic.Int64
}

// New validates the configuration and returns an empty Store.
func New(cfg Config) (*Store, error) {
	if cfg.Bucket == 0 {
		cfg.Bucket = DefaultBucket
	}
	if cfg.Bucket <= 0 {
		return nil, fmt.Errorf("%w: bucket %s is not positive", ErrConfig, cfg.Bucket)
	}
	if Session%cfg.Bucket != 0 {
		return nil, fmt.Errorf("%w: %s does not divide the %s session evenly — the last bucket "+
			"of every session would be short, and its share understated by exactly the amount "+
			"nobody would notice", ErrConfig, cfg.Bucket, Session)
	}
	if cfg.Bucket%time.Minute != 0 {
		return nil, fmt.Errorf("%w: %s is not a whole number of one-minute bars — a bucket that "+
			"splits the platform's base series cannot be reconciled against it", ErrConfig, cfg.Bucket)
	}
	if cfg.Horizon == 0 {
		cfg.Horizon = DefaultHorizon
	}
	if cfg.Horizon < Session {
		return nil, fmt.Errorf("%w: horizon %s is shorter than one %s session, so nothing but "+
			"the newest session could ever be retained and the shape would be one day's",
			ErrConfig, cfg.Horizon, Session)
	}
	if cfg.MinSessions <= 0 {
		return nil, fmt.Errorf("%w: min_sessions must be positive and has no default — how much "+
			"history a desk requires before scheduling against a shape is an execution-policy "+
			"decision, and a value invented here would make an unconfigured store answer from "+
			"whatever history it happened to hold", ErrConfig)
	}
	return &Store{
		series:      make(map[Series]*history),
		bucket:      cfg.Bucket,
		buckets:     int(Session / cfg.Bucket),
		horizon:     cfg.Horizon,
		minSessions: cfg.MinSessions,
	}, nil
}

// Bucket is the intraday bin width in force.
func (s *Store) Bucket() time.Duration { return s.bucket }

// Recomputes is how many times a shape has been rebuilt. It moves once per
// completed session per series that is then read, never per query.
func (s *Store) Recomputes() int64 { return s.recomputes.Load() }

// Late is how many prints arrived for a session already folded into a shape.
//
// NON-ZERO IS NOT FATAL AND IS NOT NOTHING. The affected sessions are missing
// volume they should have had, and the fold refuses to restate a session readers
// may already have scheduled against — so this number is the only evidence that
// a shape is short.
func (s *Store) Late() int64 { return s.late.Load() }

// Observe folds one trade.
//
// THE SESSION BOUNDARY IS DISCOVERED FROM THE DATA, not from a clock: a print
// belonging to a later session completes the one in progress, so a replay
// produces exactly the same profile as a live run. Advance is what closes the
// final session when no later print is coming.
//
// A print with no size, a non-positive size or no event time is ignored, matching
// trades.Tape: it is noise, not an execution. The PRICE is not read at all — a
// volume profile is about when quantity trades, and reading a price here would
// invite the assumption that this type says something about where.
func (s *Store) Observe(ser Series, tr trades.Trade) {
	if tr.Size == nil || tr.Size.Sign() <= 0 || tr.EventTime.IsZero() {
		return
	}
	start := tr.EventTime.UTC().Truncate(Session)

	s.mu.Lock()
	defer s.mu.Unlock()

	h, ok := s.series[ser]
	if !ok {
		h = &history{}
		s.series[ser] = h
	}
	switch {
	case h.open == nil:
		// No session is in progress, which is where Advance leaves a series after
		// an outage or a quiet stretch. A print at or before the newest COMPLETED
		// session is still late — reopening that day would restate a shape readers
		// have already scheduled against, and it would do so without the
		// out-of-order arm below ever running.
		if n := len(h.done); n > 0 && !start.After(h.done[n-1].start) {
			s.late.Add(1)
			return
		}
		h.open = newSession(start, s.buckets)
	case start.After(h.open.start):
		s.completeLocked(h)
		h.open = newSession(start, s.buckets)
	case start.Before(h.open.start):
		// A print for a session already folded into the shape. Restating it would
		// change a profile readers may have scheduled against, and a silent
		// restatement is the one thing this estate refuses everywhere.
		s.late.Add(1)
		return
	}
	i := int(tr.EventTime.UTC().Sub(start) / s.bucket)
	if i < 0 || i >= s.buckets {
		return
	}
	h.open.vol[i].Add(h.open.vol[i], tr.Size)
	h.open.total.Add(h.open.total, tr.Size)
	h.open.seen[i] = true
	h.open.prints++
}

// Advance closes every session that has fully elapsed as of now.
//
// IT IS WHAT CLOSES A SERIES WHOSE FEED HAS DIED. Observe only discovers a
// boundary when a later print arrives, so without this the last session of an
// outage would sit open forever and never enter the shape — and a profile that
// silently drops the day a pod rolled through is a shape short a session with
// nothing to say so.
//
// A session closed this way may have been observed for only part of itself, and
// this package holds no attestation that could tell. Window.Unknown is the only
// signal, and it is why the counter exists.
func (s *Store) Advance(now time.Time) {
	now = now.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.series {
		if h.open == nil || h.open.start.Add(Session).After(now) {
			continue
		}
		s.completeLocked(h)
		h.open = nil
	}
}

// completeLocked moves the open session onto the retained list and prunes past
// the horizon. The caller holds the lock and owns clearing h.open.
func (s *Store) completeLocked(h *history) {
	if h.open == nil {
		return
	}
	h.done = append(h.done, h.open)
	h.dirty = true
	s.pruneLocked(h)
}

// pruneLocked drops completed sessions older than the horizon.
//
// THE HORIZON IS MEASURED FROM THE NEWEST SESSION RETAINED, NOT FROM WALL CLOCK,
// and that is the difference between degrading and breaking. A wall-clock prune
// would empty the store the moment a feed stalled, turning "this profile is
// stale" into "there is no profile" — and those are different answers to a
// scheduler, one of which it can reason about. Staleness stays a separate,
// reported condition: see VerdictStale.
//
// IT IS A SECOND IMPLEMENTATION OF internal/risk/pricing/pit's PRUNE, AND #871
// IS THE ISSUE TO RETIRE THAT. pit is the one place this shape has been argued
// and the one place #862 was fixed; it is not imported here only because it
// lives under internal/risk/pricing, and the market-data edge depending on the
// risk pricing tree is the inversion store.Window's doc already refuses. The
// repair is to promote pit rather than to keep two horizons.
func (s *Store) pruneLocked(h *history) {
	if len(h.done) == 0 {
		return
	}
	cutoff := h.done[len(h.done)-1].start.Add(-s.horizon)
	drop := sort.Search(len(h.done), func(i int) bool { return !h.done[i].start.Before(cutoff) })
	if drop == 0 {
		return
	}
	// THE HORIZON BOUNDS len, AND WITHOUT THIS IT DOES NOT BOUND THE HEAP (#862).
	//
	// h.done[drop:] reslices the SAME backing array. Go keeps an entire array
	// alive while any slice references any part of it, so the dropped *session
	// values — each carrying one exact big.Rat per intraday bucket — stay
	// reachable from the array's prefix and are NOT collected. They are released
	// only when a later append exceeds cap and reallocates, which in steady state
	// is many sessions away, on the process whose job is to hold market state in
	// memory.
	//
	// Every retention assertion that counts sessions is blind to this; only
	// TestARetiredSessionIsReleasedWithoutWaitingForAReallocation can see it.
	// clear is O(drop), and drop is 1 in steady state.
	clear(h.done[:drop])
	h.done = h.done[drop:]
	h.dirty = true
}

// Profile answers the shape of one series as of an instant.
//
// The as-of is required and does not default to now (ErrNoAsOf), and it may not
// precede the newest completed session's end (ErrLookahead). Every other outcome
// — including every refusal — is an Answer, because "we cannot say, and here is
// how much history there is" is a result a scheduler acts on, not an error.
func (s *Store) Profile(ser Series, asOf time.Time) (Answer, error) {
	if asOf.IsZero() {
		return Answer{}, ErrNoAsOf
	}
	asOf = asOf.UTC()
	out := Answer{Series: ser, Bucket: s.bucket, AsOf: asOf}

	s.mu.Lock()
	defer s.mu.Unlock()

	h, ok := s.series[ser]
	if !ok || len(h.done) == 0 {
		// NOTHING HAS EVER COMPLETED FOR THIS SERIES. The verdict stays the zero
		// value and Shares stays nil: a series nobody has measured must not be
		// answered with a shape, and a flat one would be TWAP under another name.
		return out, nil
	}

	out.Sessions = len(h.done)
	out.Oldest = h.done[0].start
	out.Newest = h.done[len(h.done)-1].start
	newestEnd := out.Newest.Add(Session)

	if asOf.Before(newestEnd) {
		return Answer{}, fmt.Errorf("%w: as-of %s precedes the end of the session starting %s",
			ErrLookahead, asOf.Format(time.RFC3339Nano), out.Newest.Format(time.RFC3339Nano))
	}

	s.rebuildLocked(h)
	out.Window = h.window

	switch {
	case asOf.Sub(newestEnd) > s.horizon:
		// Nothing retained is inside the window the horizon describes, so serving
		// the shape would be serving a session the retention policy itself says is
		// out of scope. The counts are still reported: an operator has to be able
		// to see a feed that died from one that never started.
		out.Verdict = VerdictStale
	case len(h.done) < s.minSessions:
		out.Verdict = VerdictTooFewSessions
	default:
		out.Verdict = VerdictKnown
		out.Shares = make([]*big.Rat, len(h.shape))
		for i, sh := range h.shape {
			// THE CALLER'S OWN COPY. big.Rat is a pointer, and handing out the
			// cached one lets any reader corrupt the shape every later reader gets,
			// with nothing to say so.
			out.Shares[i] = new(big.Rat).Set(sh)
		}
	}
	return out, nil
}

// rebuildLocked recomputes the cached shape when a session has completed or been
// dropped since the last read.
//
// THE SHAPE IS THE MEAN OF EACH SESSION'S OWN DISTRIBUTION, not the pooled one.
// Pooling weights a session by its volume, so the largest day sets the schedule
// for the following month — one liquidation cascade and every subsequent order is
// sliced to a shape that happened once. Weighting sessions equally is also what
// makes MinSessions mean anything: it is a count of samples.
func (s *Store) rebuildLocked(h *history) {
	if !h.dirty && h.shape != nil {
		return
	}
	s.recomputes.Add(1)

	shape := make([]*big.Rat, s.buckets)
	for i := range shape {
		shape[i] = new(big.Rat)
	}
	w := Window{}
	contributing := 0
	for _, sess := range h.done {
		w.Buckets += s.buckets
		for i := 0; i < s.buckets; i++ {
			if sess.seen[i] {
				w.Observed++
			}
		}
		if sess.total.Sign() <= 0 {
			// STRUCTURALLY UNREACHABLE — a session exists only because a print with
			// a positive size opened it — and it is checked anyway, because the
			// alternative is a division by zero on the market-data path if that ever
			// stops being true. A session with no volume states no distribution.
			continue
		}
		contributing++
		for i := 0; i < s.buckets; i++ {
			if sess.vol[i].Sign() == 0 {
				continue
			}
			shape[i].Add(shape[i], new(big.Rat).Quo(sess.vol[i], sess.total))
		}
	}
	if contributing > 0 {
		inv := new(big.Rat).SetFrac64(1, int64(contributing))
		for i := range shape {
			shape[i].Mul(shape[i], inv)
		}
	}
	h.shape, h.window, h.dirty = shape, w, false
}

// String reports how many series are folded and the bins and horizon in force —
// the three facts that decide whether anything can be answered at all.
func (s *Store) String() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("volprofile.Store{series:%d, bucket:%s, horizon:%s, minSessions:%d}",
		len(s.series), s.bucket, s.horizon, s.minSessions)
}
