package backfill

import (
	"context"
	"errors"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
)

var (
	t0     = time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	runAt  = time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	series = Series{InstrumentID: "BTC-USDT", Symbol: "BTCUSDT", Venue: "XBIN"}
)

func d(c int64, e int32) *commonpb.Decimal { return &commonpb.Decimal{Coefficient: c, Exponent: e} }

// venueBar is what a Source returns: no venue, no resolution, and crucially NO
// KnowledgeTime — when Kanz learned a candle is the backfiller's decision, not
// the venue's.
func venueBar(minute int, closeCoef int64) store.Bar {
	return store.Bar{
		BucketStart: t0.Add(time.Duration(minute) * time.Minute),
		Open:        d(10000, -2),
		High:        d(11000, -2),
		Low:         d(9000, -2),
		Close:       d(closeCoef, -2),
		Volume:      d(15, -1),
		TradeCount:  7,
	}
}

type fakeSource struct {
	bars []store.Bar
	err  error
	got  struct {
		symbol   string
		from, to time.Time
	}
}

func (f *fakeSource) Klines(_ context.Context, symbol string, from, to time.Time) ([]store.Bar, error) {
	f.got.symbol, f.got.from, f.got.to = symbol, from, to
	return f.bars, f.err
}

func newBackfiller(t *testing.T) (*Backfiller, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	b, err := New(st, func() time.Time { return runAt })
	if err != nil {
		t.Fatal(err)
	}
	return b, st
}

// HISTORY IS STAMPED WITH WHEN WE LEARNED IT, NOT WHEN IT HAPPENED — #427's rule
// applied to backfill.
//
// A candle loaded today for a minute in January became known TODAY. Stamping it
// with the candle's own close would assert the platform knew that price live,
// and every point-in-time read would then treat data obtained months later as
// though it had been available at the time. That is the self-deception a
// bitemporal store exists to prevent.
func TestBackfilledBarsAreStampedWithTheRunTime(t *testing.T) {
	b, st := newBackfiller(t)
	src := &fakeSource{bars: []store.Bar{venueBar(0, 10500)}}

	if _, err := b.Run(context.Background(), src, series, t0, t0.Add(time.Hour)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := st.Bars(context.Background(), store.BarQuery{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: store.Resolution1m,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("stored %d bars, want 1", len(got))
	}
	if !got[0].KnowledgeTime.Equal(runAt) {
		t.Errorf("knowledge_time = %s, want the run time %s. Stamping it with the candle's own "+
			"time claims the platform knew this price live.", got[0].KnowledgeTime, runAt)
	}
	if !got[0].BucketStart.Equal(t0) {
		t.Errorf("bucket_start = %s, want %s — the OBSERVATION time is when it happened",
			got[0].BucketStart, t0)
	}
	if got[0].Venue != "XBIN" || got[0].Resolution != store.Resolution1m {
		t.Errorf("series = %s/%s, want XBIN/1m — identity comes from the caller, not the payload",
			got[0].Venue, got[0].Resolution)
	}
}

// A RE-RUN OVER UNCHANGED HISTORY WRITES NOTHING.
//
// Every write carries the run's own timestamp, so writing unconditionally would
// give each invocation a fresh version of every candle it touched — a table that
// doubles on each run, and a restatement history full of entries that restate
// nothing.
func TestARerunOverUnchangedHistoryWritesNothing(t *testing.T) {
	b, st := newBackfiller(t)
	src := &fakeSource{bars: []store.Bar{venueBar(0, 10500), venueBar(1, 10600)}}
	ctx := context.Background()

	first, err := b.Run(ctx, src, series, t0, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if first.Written != 2 {
		t.Fatalf("first run wrote %d, want 2", first.Written)
	}

	second, err := b.Run(ctx, src, series, t0, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if second.Written != 0 {
		t.Errorf("a re-run wrote %d bars — the table doubles on every invocation", second.Written)
	}
	if second.Unchanged != 2 {
		t.Errorf("unchanged = %d, want 2", second.Unchanged)
	}
	if second.Restated != 0 {
		t.Errorf("restated = %d, want 0 — nothing changed", second.Restated)
	}

	got, _ := st.Bars(ctx, store.BarQuery{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: store.Resolution1m,
	})
	if len(got) != 2 {
		t.Fatalf("series has %d buckets, want 2", len(got))
	}
}

// A VENUE THAT CHANGES ITS ANSWER IS RECORDED AS A RESTATEMENT — a new version
// beside the old rather than an overwrite, so a correction can be investigated
// against what a model may already have trained on.
func TestAChangedCandleIsWrittenAsARestatement(t *testing.T) {
	b, st := newBackfiller(t)
	ctx := context.Background()

	if _, err := b.Run(ctx, &fakeSource{bars: []store.Bar{venueBar(0, 10500)}},
		series, t0, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	later, err := New(st, func() time.Time { return runAt.Add(24 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	res, err := later.Run(ctx, &fakeSource{bars: []store.Bar{venueBar(0, 10999)}},
		series, t0, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Restated != 1 || res.Written != 1 {
		t.Fatalf("restated=%d written=%d, want 1 and 1", res.Restated, res.Written)
	}

	// The ORIGINAL is still readable as of before the correction arrived.
	early, _ := st.Bars(ctx, store.BarQuery{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: store.Resolution1m,
		AsOf: runAt.Add(time.Hour),
	})
	if len(early) != 1 || early[0].Close.GetCoefficient() != 10500 {
		t.Fatalf("as of the first run, close = %v, want the original 10500 — the correction "+
			"overwrote the evidence", early)
	}
	latest, _ := st.Bars(ctx, store.BarQuery{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: store.Resolution1m,
	})
	if latest[0].Close.GetCoefficient() != 10999 {
		t.Fatalf("latest close = %v, want the restated 10999", latest[0].Close)
	}
}

// The request uses the EXCHANGE's symbol, not the platform's id. #407 is the
// record of what conflating those two costs.
func TestTheVenueSymbolIsWhatIsRequested(t *testing.T) {
	b, _ := newBackfiller(t)
	src := &fakeSource{}

	if _, err := b.Run(context.Background(), src, series, t0, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if src.got.symbol != "BTCUSDT" {
		t.Errorf("requested %q, want the exchange symbol BTCUSDT", src.got.symbol)
	}
}

// An empty window is data, not an error: a venue that traded nothing in the
// range returns nothing, and inventing a candle would be worse.
func TestAnEmptyWindowIsNotAnError(t *testing.T) {
	b, _ := newBackfiller(t)
	res, err := b.Run(context.Background(), &fakeSource{}, series, t0, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("an empty result was an error: %v", err)
	}
	if res.Fetched != 0 || res.Written != 0 {
		t.Errorf("result = %+v, want all zero", res)
	}
}

// A fetch failure writes NOTHING. A partially loaded range is a series with a
// hole in it, and a hole is invisible downstream — it reads as a quiet market.
func TestAFetchFailureWritesNothing(t *testing.T) {
	b, st := newBackfiller(t)
	src := &fakeSource{err: errors.New("rate limited")}

	if _, err := b.Run(context.Background(), src, series, t0, t0.Add(time.Hour)); err == nil {
		t.Fatal("a failed fetch was reported as success")
	}
	got, _ := st.Bars(context.Background(), store.BarQuery{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: store.Resolution1m,
	})
	if len(got) != 0 {
		t.Errorf("a failed run wrote %d bars", len(got))
	}
}

func TestAnInvertedWindowIsRefused(t *testing.T) {
	b, _ := newBackfiller(t)
	if _, err := b.Run(context.Background(), &fakeSource{}, series, t0.Add(time.Hour), t0); err == nil {
		t.Fatal("an inverted window was accepted")
	}
}

func TestASeriesWithNoSymbolIsRefused(t *testing.T) {
	b, _ := newBackfiller(t)
	bad := Series{InstrumentID: "BTC-USDT", Venue: "XBIN"}
	if _, err := b.Run(context.Background(), &fakeSource{}, bad, t0, t0.Add(time.Hour)); err == nil {
		t.Fatal("a series with no exchange symbol was accepted")
	}
}
