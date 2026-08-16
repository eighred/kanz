package rollup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/store"
)

// Series names one candle stream to roll up.
//
// IT DELIBERATELY HAS NO VENUE SYMBOL, unlike backfill.Series. A rollup never
// talks to an exchange — its input is the store's own 1m series — so carrying the
// exchange's name for the instrument would be a field with no meaning here and
// one more thing to get wrong.
type Series struct {
	// InstrumentID is the platform's canonical id — what an order carries.
	InstrumentID string
	// Venue is the ISO 10383 MIC. A series is PER VENUE and this package never
	// reads across them; see the identity refusal in Fold.
	Venue string
}

func (s Series) validate() error {
	switch {
	case s.InstrumentID == "":
		return errors.New("rollup: instrument_id is required")
	case s.Venue == "":
		return errors.New("rollup: venue is required — a candle series is per venue, and rolling " +
			"up across venues would build a composite candle this platform does not have")
	}
	return nil
}

// Request is one rollup run: one series, one target resolution, one window.
type Request struct {
	Series

	// Target is the resolution to PRODUCE — 1h or 1d. 1m is refused: it is what is
	// ingested, not what is derived.
	Target store.Resolution

	// From is inclusive and To exclusive, on the TARGET bucket start. Both must
	// land on a target-interval boundary; a window that cut a bucket in half is
	// REFUSED rather than quietly widened, because a caller who asked for 12:30
	// and got a bar covering 12:00 has been given a different answer to the
	// question they asked.
	From, To time.Time

	// Watermark is the instant up to which the 1-minute series is believed
	// COMPLETE. A bucket is eligible only when it ENDS at or before it.
	//
	// REQUIRED, with no default — see the completeness section on Run for why a
	// default here would silently decide the one question this package exists to
	// get right.
	Watermark time.Time

	// AsOf is the KNOWLEDGE HORIZON of the whole run: the base series is read as
	// of it, the existing coarse series is compared as of it, and a derived bar
	// therefore reflects only what was known by then. Zero means "everything known
	// now", which is what a routine scheduled run wants.
	//
	// IT IS A DIFFERENT AXIS FROM Watermark and conflating the two is the mistake
	// this struct is shaped to prevent: Watermark is about OBSERVATION time (has
	// the market finished happening), AsOf is about KNOWLEDGE time (what had we
	// learned). Both reads use the same AsOf, for the reason indicator.Source
	// states one package over: bounding one axis and not the other still leaks,
	// and it leaks invisibly because the numbers stay plausible.
	AsOf time.Time
}

// ErrIncompleteInput refuses a run that cannot say which buckets are finished.
var ErrIncompleteInput = errors.New("rollup: watermark is required")

// ErrDerivationConflict reports a coarse bar the store already holds at the same
// knowledge time with DIFFERENT numbers.
//
// IT IS AN ERROR AND NOT A WRITE, because a write would do nothing: PutBars is a
// no-op on an identical (instrument, venue, resolution, bucket_start,
// knowledge_time) tuple — Postgres does ON CONFLICT DO NOTHING — so the stored,
// wrong value would silently WIN and the run would report success. The only way
// this happens is that the fold's arithmetic changed since the bar was written,
// which is an operator-level event: the derived series has to be rebuilt under a
// new knowledge time, and that is a decision a human makes, not one a scheduled
// job makes silently at 03:00.
var ErrDerivationConflict = errors.New("rollup: derived bar conflicts with a stored one at the " +
	"same knowledge time")

// Result reports what a run did.
type Result struct {
	// Buckets is how many target buckets the window contained AND the watermark
	// said were finished.
	Buckets int
	// Written is how many coarse bars were new or changed.
	Written int
	// Unchanged is how many the store already agreed with. On a re-run over an
	// unchanged range this equals Buckets minus Empty, and Written is zero.
	Unchanged int
	// Restated is how many the store held a DIFFERENT answer for — a subset of
	// Written, and the number worth alerting on. It means a constituent minute was
	// restated after this bucket was first derived; the previous derived bar is
	// still there to compare against.
	Restated int
	// Incomplete is how many buckets in the window the watermark said were not
	// finished. They are SKIPPED, not written — see Run.
	Incomplete int
	// Empty is how many finished buckets held no 1m bar at all. An OHLC bar with
	// no prices cannot be constructed, so nothing is written and the coarse series
	// has a hole exactly where the base series does.
	//
	// A NON-ZERO VALUE ON A LIQUID INSTRUMENT IS A GAP, not a quiet market, and it
	// is the one signal this package can offer about coverage — see the completeness
	// note on Run for why it cannot offer a better one.
	Empty int
}

// Roller derives the coarse series into the bar store.
//
// IT HOLDS NO CLOCK, on purpose. Every time-dependent decision it makes comes
// from a field on the Request, so a run is reproducible and a test does not have
// to arrange for "now" to be somewhere convenient. The one thing a clock would be
// used for — deciding whether a bucket has finished — is the decision that must
// NOT be made from wall time alone; see Run.
type Roller struct {
	store store.BarStore
}

// New wraps a bar store.
func New(st store.BarStore) (*Roller, error) {
	if st == nil {
		return nil, errors.New("rollup: store is required")
	}
	return &Roller{store: st}, nil
}

// Run derives every finished bucket in the window and writes only what the store
// does not already agree with.
//
// # COMPLETENESS: what it is, and what this cannot see
//
// Rolling a partial bucket up produces a bar that is byte-for-byte the shape of a
// finished one, is wrong, and is then CACHED FOREVER — the store is append-only
// by (bucket, knowledge_time), so nothing revisits it and no consumer can tell.
// That makes the completeness test the most consequential thing in this package.
//
// THE OBVIOUS TEST DOES NOT WORK. "1440 constituents" is not completeness: a
// minute in which nothing traded produces NO BAR AT ALL, so a genuinely quiet day
// is indistinguishable from a day whose ingestion died — the store has no way to
// say "we looked at 12:37 and there was nothing", only "there is no row". Neither
// is "the last minute exists", for the same reason: the last minute may be the
// quiet one.
//
// So completeness is asserted by the CALLER, through Watermark: the instant up to
// which the 1-minute series is believed complete. A bucket is eligible only when
// it ENDS at or before it. There is no default and a zero Watermark is REFUSED —
// a default here would be a value that looks healthy and silently decides the
// single question this package exists to get right.
//
// WHAT THIS MISSES, stated plainly: the watermark is an assertion, and it is only
// as good as whatever produced it. If ingestion was down for an hour in the middle
// of a fully-elapsed day, the watermark still says the day is finished, and the
// day rolls up missing that hour — a bar with a plausible open, a plausible close,
// and a volume short by an unknowable amount. Nothing in the bar series can detect
// this, because the missing hour looks exactly like a quiet hour. The fix does not
// belong here: it belongs to an ingestion-coverage record that states which
// intervals were actually OBSERVED, which this platform does not yet have. Until
// then Result.Empty is the only smoke signal available, and it only fires when a
// whole bucket is missing.
//
// # RE-RUNNING IS SAFE, AND IS THE ONLY REPAIR MECHANISM
//
// A derived bar's KnowledgeTime is a pure function of its constituents (see Fold),
// so a re-run over unchanged input produces the identical tuple and PutBars is a
// no-op — this reports Unchanged and writes nothing. When a 1m bar has been
// RESTATED since the last run, its later KnowledgeTime flows into the fold, the
// derived bar changes, and it is written BESIDE the old one under the new stamp:
// a backtest as of a moment before the restatement still reads the old derived
// bar, which is correct.
//
// WHAT A RE-RUN DOES NOT FIX. Nothing invalidates a derived bar automatically:
// if a minute from January is restated in August, the January day is stale until
// somebody re-runs that window. There is no signal from the store that this
// happened — the closest thing is backfill.Result.Restated, which is the count a
// loader should turn into a re-run of the affected range. And a bar rolled up from
// a bucket that was silently short of minutes is never repaired by a re-run
// either, unless the missing minutes have since arrived; the run cannot tell that
// they were missing, only that today's answer differs from the stored one.
func (r *Roller) Run(ctx context.Context, req Request) (Result, error) {
	var res Result
	if err := req.validate(); err != nil {
		return res, err
	}
	interval, _ := req.Target.Interval()

	// The last bucket START the watermark permits: a bucket [s, s+interval) is
	// finished only when s+interval <= Watermark, which is exactly s < the
	// watermark truncated down to a boundary.
	limit := req.Watermark.UTC().Truncate(interval)
	horizon := req.To.UTC()
	if limit.Before(horizon) {
		res.Incomplete = int(horizon.Sub(limit) / interval)
		horizon = limit
	}
	from := req.From.UTC()
	if !from.Before(horizon) {
		// Every bucket asked for is still open. Not an error: a scheduled run that
		// arrives before the hour has closed has nothing to do, and returning a
		// failure would make an idle run indistinguishable from a broken one.
		return res, nil
	}
	res.Buckets = int(horizon.Sub(from) / interval)

	// ONE READ FOR THE WHOLE WINDOW, not one per bucket. The point of this package
	// is that a consumer stops paying for 40,320 rows; a driver that issued a query
	// per bucket would move that cost rather than remove it.
	minutes, err := r.store.Bars(ctx, store.BarQuery{
		InstrumentID: req.InstrumentID,
		Venue:        req.Venue,
		Resolution:   store.Resolution1m,
		From:         from,
		To:           horizon,
		AsOf:         req.AsOf,
	})
	if err != nil {
		return res, fmt.Errorf("rollup: read 1m %s %s: %w", req.Venue, req.InstrumentID, err)
	}

	// The SAME horizon on the comparison read. Reading the existing coarse series
	// without AsOf would compare today's derivation against a version derived from
	// knowledge this run is not allowed to see, and would then "restate" it back to
	// the narrower answer on every run.
	known, err := r.store.Bars(ctx, store.BarQuery{
		InstrumentID: req.InstrumentID,
		Venue:        req.Venue,
		Resolution:   req.Target,
		From:         from,
		To:           horizon,
		AsOf:         req.AsOf,
	})
	if err != nil {
		return res, fmt.Errorf("rollup: read %s %s %s: %w", req.Target, req.Venue, req.InstrumentID, err)
	}
	stored := make(map[int64]store.Bar, len(known))
	for _, k := range known {
		stored[k.BucketStart.UTC().UnixNano()] = k
	}

	grouped := make(map[int64][]store.Bar, res.Buckets)
	for _, m := range minutes {
		b := m.BucketStart.UTC().Truncate(interval)
		grouped[b.UnixNano()] = append(grouped[b.UnixNano()], m)
	}

	// Walked as a GRID rather than over the groups, so a bucket with no bars at all
	// is counted instead of silently absent — "nothing configured" and "checked,
	// and fine" must not look the same, and a coarse series that is simply missing
	// a day looks like neither.
	var write []store.Bar
	for start := from; start.Before(horizon); start = start.Add(interval) {
		mins := grouped[start.UnixNano()]
		if len(mins) == 0 {
			res.Empty++
			continue
		}
		bar, err := Fold(req.Target, start, mins)
		if err != nil {
			return res, err
		}
		prev, seen := stored[start.UnixNano()]
		switch {
		case !seen:
			write = append(write, bar)
		case sameCandle(prev, bar):
			// Values agree, so there is nothing to record even if the knowledge
			// stamp moved. Writing anyway would add a restatement of nothing on
			// every run and bury the real ones.
			res.Unchanged++
		case prev.KnowledgeTime.Equal(bar.KnowledgeTime):
			return res, fmt.Errorf("%w: %s %s at %s — the stored bar and the freshly derived one "+
				"carry the same knowledge time and different numbers, so the fold itself changed. "+
				"A write would be silently discarded as a duplicate key and the stored value would "+
				"stand; the derived series must be rebuilt under a new knowledge time instead",
				ErrDerivationConflict, req.Target, req.InstrumentID, start)
		default:
			res.Restated++
			write = append(write, bar)
		}
	}

	if len(write) == 0 {
		return res, nil
	}
	// Ordered by bucket because the grid walk is, so a re-run submits a
	// byte-identical batch — the property that makes PutBars' idempotency reachable
	// rather than theoretical.
	if err := r.store.PutBars(ctx, write); err != nil {
		return res, fmt.Errorf("rollup: write %s %s %s: %w", req.Target, req.Venue, req.InstrumentID, err)
	}
	res.Written = len(write)
	return res, nil
}

func (req Request) validate() error {
	if err := req.Series.validate(); err != nil {
		return err
	}
	interval, ok := req.Target.Interval()
	if !ok {
		return fmt.Errorf("rollup: target %q is not a resolution this platform stores", req.Target)
	}
	base, _ := store.Resolution1m.Interval()
	if interval <= base {
		return fmt.Errorf("rollup: target %q is not coarser than the 1m base series — 1m is "+
			"INGESTED, and deriving it here would give one candle two sources", req.Target)
	}
	if req.From.IsZero() || req.To.IsZero() {
		return errors.New("rollup: from and to are required")
	}
	if !req.From.Before(req.To) {
		return fmt.Errorf("rollup: window [%s, %s) is empty or inverted", req.From, req.To)
	}
	for _, f := range []struct {
		name string
		t    time.Time
	}{{"from", req.From}, {"to", req.To}} {
		u := f.t.UTC()
		if !u.Equal(u.Truncate(interval)) {
			return fmt.Errorf("rollup: %s %s is not on a %s boundary — a window that cut a bucket "+
				"in half would either drop it or widen the answer past what was asked for, and both "+
				"are silent", f.name, u, req.Target)
		}
	}
	if req.Watermark.IsZero() {
		return fmt.Errorf("%w: without it there is no way to tell a finished bucket from one still "+
			"filling, and a partial bucket rolled up is indistinguishable from a whole one and is "+
			"kept forever", ErrIncompleteInput)
	}
	return nil
}

// sameTradeCount compares two counts BY VALUE, treating "neither was reported" as
// equal (#432).
//
// THE OBVIOUS EXPRESSION COMPILES AND IS WRONG: TradeCount is a *int64, so
// `a.TradeCount == b.TradeCount` compares ADDRESSES and two bars both reporting 42
// would look different on every run.
func sameTradeCount(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// sameCandle reports whether a freshly derived bar says the same thing as the
// stored one, comparing EXACTLY through dec.Cmp rather than a float conversion.
//
// DUPLICATED FROM backfill.sameCandle, knowingly, and it should not stay that way.
// The house rule is that shared code is promoted on the SECOND consumer, and this
// is the second consumer — but promoting it means editing internal/marketdata/
// backfill, which is outside this change. The right home is beside store.Bar
// itself, since "do these two versions of a bar agree" is a property of the type
// and both callers use it for the same purpose: keeping a re-run from writing a
// restatement of nothing. Until then, a fix to one of these must be applied to
// both — which is precisely the failure mode the rule exists to prevent, so this
// note is the debt, not the excuse.
func sameCandle(a, b store.Bar) bool {
	return dec.Cmp(a.Open, b.Open) == 0 &&
		dec.Cmp(a.High, b.High) == 0 &&
		dec.Cmp(a.Low, b.Low) == 0 &&
		dec.Cmp(a.Close, b.Close) == 0 &&
		dec.Cmp(a.Volume, b.Volume) == 0 &&
		sameTradeCount(a.TradeCount, b.TradeCount)
}
