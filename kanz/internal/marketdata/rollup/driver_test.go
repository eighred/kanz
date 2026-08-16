package rollup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/store"
)

// recordingStore is the narrow BarStore seam with the queries and writes kept.
//
// It DELEGATES to store.Memory rather than faking the reads, because the two
// behaviours under test here — the knowledge horizon collapsing a bucket to its
// latest version, and PutBars being a no-op on an identical tuple — are the
// store's semantics, and a hand-rolled stub would be asserting a store this
// platform does not have. No database is involved.
type recordingStore struct {
	*store.Memory
	queries []store.BarQuery
	batches [][]store.Bar
	putErr  error
}

func newStore() *recordingStore { return &recordingStore{Memory: store.NewMemory()} }

func (r *recordingStore) Bars(ctx context.Context, q store.BarQuery) ([]store.Bar, error) {
	r.queries = append(r.queries, q)
	return r.Memory.Bars(ctx, q)
}

func (r *recordingStore) PutBars(ctx context.Context, bars []store.Bar) error {
	if r.putErr != nil {
		return r.putErr
	}
	batch := make([]store.Bar, len(bars))
	copy(batch, bars)
	r.batches = append(r.batches, batch)
	return r.Memory.PutBars(ctx, bars)
}

// queriesFor returns the queries this run issued against one resolution.
func (r *recordingStore) queriesFor(res store.Resolution) []store.BarQuery {
	var out []store.BarQuery
	for _, q := range r.queries {
		if q.Resolution == res {
			out = append(out, q)
		}
	}
	return out
}

var (
	series    = Series{InstrumentID: "BTC-USDT", Venue: "XBIN"}
	hourAfter = hour0.Add(time.Hour)
)

// request is a complete 1h run over the single hour the fixtures live in, with
// the watermark past its end so the hour counts as finished.
func request() Request {
	return Request{
		Series:    series,
		Target:    store.Resolution1h,
		From:      hour0,
		To:        hourAfter,
		Watermark: hourAfter,
	}
}

func seed(t *testing.T, st *recordingStore, bars ...store.Bar) {
	t.Helper()
	if err := st.Memory.PutBars(context.Background(), bars); err != nil {
		t.Fatalf("seeding the base series: %v", err)
	}
}

func run(t *testing.T, st *recordingStore, req Request) Result {
	t.Helper()
	r, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func coarse(t *testing.T, st *recordingStore, res store.Resolution, asOf time.Time) []store.Bar {
	t.Helper()
	got, err := st.Memory.Bars(context.Background(), store.BarQuery{
		InstrumentID: series.InstrumentID,
		Venue:        series.Venue,
		Resolution:   res,
		AsOf:         asOf,
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A FINISHED HOUR IS DERIVED AND WRITTEN, with the same numbers the pure fold
// produces. This is the baseline the refusals below are refusals against.
func TestAFinishedHourIsWrittenToTheCoarseSeries(t *testing.T) {
	st := newStore()
	seed(t, st, threeMinutes()...)

	res := run(t, st, request())

	if res.Written != 1 || res.Buckets != 1 || res.Incomplete != 0 {
		t.Fatalf("result = %+v, want 1 bucket written and none incomplete", res)
	}
	got := coarse(t, st, store.Resolution1h, time.Time{})
	if len(got) != 1 {
		t.Fatalf("stored %d 1h bars, want 1", len(got))
	}
	want := mustFold(t, store.Resolution1h, hour0, threeMinutes())
	if !store.SameCandle(want, got[0]) || !got[0].KnowledgeTime.Equal(want.KnowledgeTime) {
		t.Errorf("the driver wrote a different bar from the fold: %+v vs %+v", got[0], want)
	}
}

// AN UNFINISHED BUCKET IS NOT WRITTEN.
//
// The hour has minutes in it and they are perfectly good minutes — a fold over
// them succeeds and returns a bar that is indistinguishable from a complete one.
// That is exactly why the watermark decides and not the data: the bar would be
// wrong, would validate, and would be kept forever, because the store is
// append-only by (bucket, knowledge_time) and nothing revisits it.
func TestABucketTheWatermarkHasNotReachedIsNotWritten(t *testing.T) {
	st := newStore()
	seed(t, st, threeMinutes()...)

	req := request()
	req.Watermark = hour0.Add(59 * time.Minute) // one minute short of the hour's end

	res := run(t, st, req)

	if res.Written != 0 {
		t.Errorf("wrote %d bars for an hour that has not finished", res.Written)
	}
	if res.Incomplete != 1 {
		t.Errorf("Incomplete = %d, want 1 — the skip must be REPORTED, not silent", res.Incomplete)
	}
	if got := coarse(t, st, store.Resolution1h, time.Time{}); len(got) != 0 {
		t.Errorf("the coarse series holds %d bars, want 0: a partial bucket was rolled up as if it "+
			"were whole", len(got))
	}
}

// A MISSING WATERMARK IS REFUSED, NOT DEFAULTED. "Nothing configured" and
// "checked, and fine" must never look the same, and a default here would silently
// decide the one question this package exists to get right.
func TestARunWithNoWatermarkIsRefused(t *testing.T) {
	st := newStore()
	seed(t, st, threeMinutes()...)
	r, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	req := request()
	req.Watermark = time.Time{}

	if _, err := r.Run(context.Background(), req); !errors.Is(err, ErrIncompleteInput) {
		t.Fatalf("err = %v, want ErrIncompleteInput", err)
	}
	if len(st.batches) != 0 {
		t.Error("a refused run still wrote to the store")
	}
}

// THE BASE READ IS BOUNDED ON THE KNOWLEDGE AXIS, AND SO IS THE COMPARISON READ.
//
// BarQuery.AsOf is what makes the derived series honest: without it a rollup "as
// of" a past date folds corrections that arrived afterwards, and the resulting
// coarse bar is a candle that could not have been computed at the time. Bounding
// the observation window alone leaks invisibly, because the numbers stay
// plausible — the same trap indicator.Source's read is shaped to avoid.
func TestBothReadsAreBoundedByTheKnowledgeHorizon(t *testing.T) {
	st := newStore()
	seed(t, st, threeMinutes()...)

	horizon := learned.Add(time.Hour)
	req := request()
	req.AsOf = horizon
	run(t, st, req)

	base := st.queriesFor(store.Resolution1m)
	if len(base) != 1 {
		t.Fatalf("issued %d reads of the 1m series, want exactly 1 for the whole window", len(base))
	}
	if !base[0].AsOf.Equal(horizon) {
		t.Errorf("the 1m read carried AsOf %s, want %s — an unbounded read folds corrections that "+
			"arrived after the horizon", base[0].AsOf, horizon)
	}
	existing := st.queriesFor(store.Resolution1h)
	if len(existing) != 1 {
		t.Fatalf("issued %d reads of the coarse series, want 1", len(existing))
	}
	if !existing[0].AsOf.Equal(horizon) {
		t.Errorf("the coarse comparison read carried AsOf %s, want %s — comparing against a version "+
			"derived from knowledge this run may not see would restate it away on every run",
			existing[0].AsOf, horizon)
	}
}

// A RE-RUN OVER UNCHANGED INPUT WRITES NOTHING.
//
// It holds because the derived KnowledgeTime is a pure function of the
// constituents: the second run derives the identical tuple, the comparison finds
// the store already agrees, and no write is issued at all. Had the stamp been the
// run time, every re-run would have written a fresh version of every bucket —
// a table that doubles per invocation and a restatement history full of entries
// restating nothing.
func TestARerunWritesNothingNew(t *testing.T) {
	st := newStore()
	seed(t, st, threeMinutes()...)

	first := run(t, st, request())
	if first.Written != 1 {
		t.Fatalf("first run wrote %d bars, want 1", first.Written)
	}

	second := run(t, st, request())

	if second.Written != 0 {
		t.Errorf("the re-run wrote %d bars, want 0", second.Written)
	}
	if second.Unchanged != 1 {
		t.Errorf("Unchanged = %d, want 1", second.Unchanged)
	}
	if len(st.batches) != 1 {
		t.Errorf("PutBars was called %d times across two identical runs, want 1 — a re-run must not "+
			"reach the store at all", len(st.batches))
	}
	if got := coarse(t, st, store.Resolution1h, time.Time{}); len(got) != 1 {
		t.Errorf("the coarse series holds %d versions after two identical runs, want 1", len(got))
	}
}

// A RESTATED MINUTE PRODUCES A NEW VERSION OF THE HOUR, BESIDE THE OLD ONE.
//
// This is the whole bitemporal contract in one test: the correction is visible as
// a correction, and a read as of an instant BEFORE it still returns the bar that
// was derivable then. A store that overwrote would make every historical
// evaluation quietly optimistic, in the direction nobody checks.
func TestARestatedMinuteRestatesTheHourWithoutErasingIt(t *testing.T) {
	st := newStore()
	seed(t, st, threeMinutes()...)
	run(t, st, request())

	// The venue corrects the last minute's close, learned three days later.
	corrected := time.Date(2026, 3, 7, 9, 0, 0, 0, time.UTC)
	restated := minute(hour0, minuteSpec{
		at: 2, open: 19000, high: 19500, low: 5000, close: 17000,
		volume: 30, trades: ptrTo(7), knownAt: corrected,
	})
	seed(t, st, restated)

	res := run(t, st, request())

	if res.Restated != 1 || res.Written != 1 {
		t.Fatalf("result = %+v, want one restated bar written", res)
	}
	now := coarse(t, st, store.Resolution1h, time.Time{})
	if len(now) != 1 {
		t.Fatalf("the latest view holds %d bars, want 1", len(now))
	}
	if now[0].Close.GetCoefficient() != 17000 {
		t.Errorf("the current 1h close is %d, want the corrected 17000",
			now[0].Close.GetCoefficient())
	}
	if !now[0].KnowledgeTime.Equal(corrected) {
		t.Errorf("the restated hour is stamped %s, want the correction's own knowledge time %s",
			now[0].KnowledgeTime, corrected)
	}

	before := coarse(t, st, store.Resolution1h, corrected.Add(-time.Nanosecond))
	if len(before) != 1 {
		t.Fatalf("as of before the correction the store holds %d bars, want the original 1", len(before))
	}
	if before[0].Close.GetCoefficient() != 15000 {
		t.Errorf("a read as of before the correction returns close %d, want the original 15000 — the "+
			"restatement leaked backwards", before[0].Close.GetCoefficient())
	}
}

// A BUCKET WITH NO MINUTES AT ALL PRODUCES NOTHING, AND SAYS SO.
//
// There is no price to open at, so the only alternative to skipping is inventing
// one. The count is reported because a hole in the base series and a genuinely
// dead market are indistinguishable here — Result.Empty is the only smoke signal
// this package can offer about coverage.
func TestAnEmptyBucketIsSkippedAndCounted(t *testing.T) {
	st := newStore()
	seed(t, st, threeMinutes()...)

	req := request()
	req.To = hour0.Add(3 * time.Hour)
	req.Watermark = req.To

	res := run(t, st, req)

	if res.Buckets != 3 {
		t.Fatalf("Buckets = %d, want 3", res.Buckets)
	}
	if res.Empty != 2 {
		t.Errorf("Empty = %d, want 2 — the two hours with no minutes must be counted, not silently "+
			"absent", res.Empty)
	}
	if res.Written != 1 {
		t.Errorf("Written = %d, want 1", res.Written)
	}
}

// A WINDOW THAT CUTS A BUCKET IN HALF IS REFUSED. Widening it would answer a
// different question from the one asked, and narrowing it would drop a bucket —
// both silently.
//
// The unaligned edge is To rather than From, deliberately: an unaligned From is
// caught a second time by the fold, so it would pass this test with the request
// validation removed. A half-open trailing bucket reaches the grid walk and is
// simply skipped as empty, which is the silent outcome worth refusing.
func TestAnUnalignedWindowIsRefused(t *testing.T) {
	st := newStore()
	seed(t, st, threeMinutes()...)
	r, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	req := request()
	req.To = hour0.Add(90 * time.Minute)
	req.Watermark = req.To

	if _, err := r.Run(context.Background(), req); err == nil {
		t.Fatal("a window ending mid-bucket was accepted")
	}
}

// THE DAILY SERIES IS A UTC DAY, and it folds the same way.
//
// Worth its own case because 1d is where the row-count argument lives: 28 days of
// minutes is ~40,320 rows per instrument per call, and internal/risk/compute walks
// the book twice per risk request.
func TestADayFoldsFromMinutesSpreadAcrossIt(t *testing.T) {
	st := newStore()
	seed(t, st,
		minute(day0, minuteSpec{at: 0, open: 10000, high: 10100, low: 9900, close: 10050, volume: 10, trades: ptrTo(2)}),
		minute(day0, minuteSpec{at: 725, open: 10050, high: 30000, low: 10000, close: 20000, volume: 20, trades: ptrTo(4)}),
		minute(day0, minuteSpec{at: 1439, open: 20000, high: 20100, low: 1000, close: 12345, volume: 30, trades: ptrTo(6)}),
	)

	res := run(t, st, Request{
		Series:    series,
		Target:    store.Resolution1d,
		From:      day0,
		To:        day0.Add(24 * time.Hour),
		Watermark: day0.Add(24 * time.Hour),
	})
	if res.Written != 1 {
		t.Fatalf("result = %+v, want one day written", res)
	}

	got := coarse(t, st, store.Resolution1d, time.Time{})[0]
	if got.Open.GetCoefficient() != 10000 || got.Close.GetCoefficient() != 12345 {
		t.Errorf("open/close = %d/%d, want 10000/12345", got.Open.GetCoefficient(), got.Close.GetCoefficient())
	}
	if got.High.GetCoefficient() != 30000 || got.Low.GetCoefficient() != 1000 {
		t.Errorf("high/low = %d/%d, want 30000/1000 — both live in the middle and the last minute",
			got.High.GetCoefficient(), got.Low.GetCoefficient())
	}
	if got.TradeCount == nil || *got.TradeCount != 12 {
		t.Errorf("trade_count = %v, want 12", got.TradeCount)
	}
	if !got.BucketStart.Equal(day0) {
		t.Errorf("bucket_start = %s, want the UTC day %s", got.BucketStart, day0)
	}
}

// A STORED BAR THAT DISAGREES AT THE SAME KNOWLEDGE TIME IS AN ERROR, NOT A WRITE.
//
// PutBars is a no-op on an identical (instrument, venue, resolution, bucket_start,
// knowledge_time) tuple, so writing here would be silently discarded and the
// WRONG stored value would stand while the run reported success. The only way to
// reach this state is that the fold's arithmetic changed, which is an operator
// decision, not something a scheduled job resolves at 03:00.
func TestABarThatDisagreesAtTheSameKnowledgeTimeIsRefused(t *testing.T) {
	st := newStore()
	seed(t, st, threeMinutes()...)

	poisoned := mustFold(t, store.Resolution1h, hour0, threeMinutes())
	poisoned.Close = poisoned.High // a different answer under the same stamp
	seed(t, st, poisoned)

	r, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), request()); !errors.Is(err, ErrDerivationConflict) {
		t.Fatalf("err = %v, want ErrDerivationConflict — a write here would be dropped as a "+
			"duplicate key and the stored value would silently win", err)
	}
}
