// Memory bar store tests (#425).
//
// WITHOUT THIS FILE THE BAR STORE HAS NO COVERAGE ON A BARE CHECKOUT. Every
// other bar test is gated on TEST_POSTGRES_URL and SKIPS when it is unset — the
// trap CLAUDE.md names — and the ingest tests run against a fake writer, not a
// store. So on a clean clone the entire OHLCV feature would report ok with not
// one line of either store executed.
//
// These assert the SAME PROPERTIES as bar_postgres_test.go, deliberately. The
// memory store's point-in-time collapse is hand-written Go where Postgres uses
// DISTINCT ON, and bar_memory.go's own comment claims the two contracts match.
// A claim nothing checks is how they drift, and the drift is silent: the suite
// would go on proving a store the database does not implement.
package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// EVERY FIELD SURVIVES THE ROUND TRIP. The defect #425 closes is that ingest
// kept the close and dropped open, high, low, volume and trade_count — so a test
// that checked only the close would pass on the very bug being fixed.
func TestMemoryBarsRoundTripEveryField(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	want := bar(0, 10500, 1)
	if err := m.PutBars(ctx, []Bar{want}); err != nil {
		t.Fatalf("PutBars: %v", err)
	}
	got, err := m.Bars(ctx, BarQuery{InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m})
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("bars = %d, want 1", len(got))
	}
	g := got[0]
	for _, f := range []struct {
		name      string
		got, want int64
	}{
		{"open", g.Open.GetCoefficient(), want.Open.GetCoefficient()},
		{"high", g.High.GetCoefficient(), want.High.GetCoefficient()},
		{"low", g.Low.GetCoefficient(), want.Low.GetCoefficient()},
		{"close", g.Close.GetCoefficient(), want.Close.GetCoefficient()},
		{"volume", g.Volume.GetCoefficient(), want.Volume.GetCoefficient()},
		{"trade_count", g.TradeCount, want.TradeCount},
	} {
		if f.got != f.want {
			t.Errorf("%s = %d, want %d — this is a field the old ingest discarded", f.name, f.got, f.want)
		}
	}
	if !g.BucketStart.Equal(want.BucketStart) {
		t.Errorf("bucket_start = %s, want %s", g.BucketStart, want.BucketStart)
	}
}

// A RESTATEMENT COEXISTS WITH THE ORIGINAL, and a read as of the earlier moment
// still sees the earlier value.
//
// This is the load-bearing property of the store, and the one place the memory
// implementation is most likely to diverge: "last write wins" over a map would
// pass a naive round-trip test and silently answer every historical read with
// today's correction. barCount proves both versions are RETAINED, not merely
// that the newest is returned.
func TestMemoryBarsAreBitemporal(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	original := bar(0, 10500, 1)  // close 105.00, known at T+1m
	restated := bar(0, 10900, 30) // same bucket, close 109.00, known at T+30m
	if err := m.PutBars(ctx, []Bar{original, restated}); err != nil {
		t.Fatalf("PutBars: %v", err)
	}
	if n := m.barCount(); n != 2 {
		t.Fatalf("stored versions = %d, want 2 — the restatement OVERWROTE the original instead of "+
			"coexisting with it, and the original is now unrecoverable", n)
	}

	early, err := m.Bars(ctx, BarQuery{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m,
		AsOf: barT0.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(early) != 1 || early[0].Close.GetCoefficient() != 10500 {
		t.Fatalf("as-of T+5m close = %v, want 10500 — a correction that arrived at T+30m leaked "+
			"backwards into a read that predates it, which makes every backtest quietly optimistic", early)
	}

	latest, err := m.Bars(ctx, BarQuery{InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m})
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(latest) != 1 || latest[0].Close.GetCoefficient() != 10900 {
		t.Fatalf("latest close = %v, want 10900", latest)
	}
}

// THE COLLAPSE IS BY KNOWLEDGE TIME, NOT INSERTION ORDER. Bars arrive out of
// order after a gap backfill or a replay, so a store that kept "the last one
// put" would serve a stale candle whenever a correction was loaded before the
// bar it corrects.
func TestMemoryBarsCollapseByKnowledgeTimeNotPutOrder(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	// The NEWER version is written FIRST, the older second.
	if err := m.PutBars(ctx, []Bar{bar(0, 10900, 30), bar(0, 10500, 1)}); err != nil {
		t.Fatalf("PutBars: %v", err)
	}
	got, err := m.Bars(ctx, BarQuery{InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m})
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(got) != 1 || got[0].Close.GetCoefficient() != 10900 {
		t.Fatalf("close = %v, want 10900 — the store returned the last bar PUT rather than the "+
			"latest KNOWN, so a replay that loads a correction first serves a stale candle", got)
	}
}

// A RE-PUT OF THE IDENTICAL TUPLE IS A NO-OP, matching the Postgres
// ON CONFLICT DO NOTHING. Redelivery is normal here — a nacked bus delivery is
// retried — so an ingest that ran twice must not double a bar into the series.
func TestMemoryBarsPutIsIdempotent(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	b := bar(0, 10500, 1)
	for i := range 3 {
		if err := m.PutBars(ctx, []Bar{b}); err != nil {
			t.Fatalf("PutBars #%d: %v", i, err)
		}
	}
	if n := m.barCount(); n != 1 {
		t.Fatalf("stored versions = %d, want 1 — a redelivered bar was stored again", n)
	}
}

// THE VENUE IS PART OF THE SERIES' IDENTITY. Two exchanges' candles for the same
// minute must not collapse: they have different books and different closes, and
// a model trained on a blend of them executes against neither.
func TestMemoryBarsAreSeparatedByVenue(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	okx := bar(0, 10600, 1)
	okx.Venue = "XOKX"
	if err := m.PutBars(ctx, []Bar{bar(0, 10500, 1), okx}); err != nil {
		t.Fatalf("PutBars: %v", err)
	}
	for _, tc := range []struct {
		venue string
		want  int64
	}{{"XBIN", 10500}, {"XOKX", 10600}} {
		got, err := m.Bars(ctx, BarQuery{InstrumentID: "BTC-USDT", Venue: tc.venue, Resolution: Resolution1m})
		if err != nil {
			t.Fatalf("Bars(%s): %v", tc.venue, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: bars = %d, want 1 — the venues collapsed into one series", tc.venue, len(got))
		}
		if got[0].Close.GetCoefficient() != tc.want {
			t.Errorf("%s close = %d, want %d — this venue is being served another venue's candle",
				tc.venue, got[0].Close.GetCoefficient(), tc.want)
		}
	}
}

// Resolutions do not collapse either: a 1m candle and the 1h candle covering it
// are different observations of the same market at the same instant.
func TestMemoryBarsAreSeparatedByResolution(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	hour := bar(0, 10700, 1)
	hour.Resolution = Resolution1h
	if err := m.PutBars(ctx, []Bar{bar(0, 10500, 1), hour}); err != nil {
		t.Fatalf("PutBars: %v", err)
	}
	got, err := m.Bars(ctx, BarQuery{InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m})
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(got) != 1 || got[0].Close.GetCoefficient() != 10500 {
		t.Fatalf("1m series = %v, want only the 1m candle", got)
	}
}

// The window is [From, To) and the result ascends by bucket_start, so
// consecutive queries neither drop nor double-count a bucket — the property
// every rolling indicator is built on.
func TestMemoryBarsWindowIsHalfOpenAndOrdered(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	// Put descending, to prove the ORDER comes from the store rather than from
	// the order of insertion.
	if err := m.PutBars(ctx, []Bar{bar(2, 10300, 1), bar(1, 10200, 1), bar(0, 10100, 1)}); err != nil {
		t.Fatalf("PutBars: %v", err)
	}
	got, err := m.Bars(ctx, BarQuery{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m,
		From: barT0, To: barT0.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("bars = %d, want 2 (buckets 0 and 1; bucket 2 starts exactly at To)", len(got))
	}
	if !got[0].BucketStart.Before(got[1].BucketStart) {
		t.Error("bars are not ordered by bucket_start ascending")
	}
	if got[0].Close.GetCoefficient() != 10100 || got[1].Close.GetCoefficient() != 10200 {
		t.Errorf("window = %d, %d; want 10100, 10200",
			got[0].Close.GetCoefficient(), got[1].Close.GetCoefficient())
	}
}

// A BAR NO MARKET PRODUCED IS REFUSED, AND THE WHOLE BATCH FAILS WITH IT.
//
// Half a batch written is a hole in a series, and a hole is invisible
// downstream — an indicator simply computes a different number and nothing says
// why. The memory store must refuse exactly as strictly as Postgres, because a
// laxer contract here means the suite proves a store the database does not
// implement.
func TestMemoryBarsRefuseAnImpossibleCandle(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	bad := bar(1, 10500, 1)
	bad.Low = d(12000, -2) // low above high

	err := m.PutBars(ctx, []Bar{bar(0, 10100, 1), bad})
	if err == nil {
		t.Fatal("a candle with low above high was accepted")
	}
	if !errors.Is(err, ErrInvalidBar) {
		t.Errorf("error = %v, want ErrInvalidBar — callers cannot distinguish a malformed bar from "+
			"a store outage, and the two need opposite handling", err)
	}
	if n := m.barCount(); n != 0 {
		t.Fatalf("stored versions = %d, want 0 — the valid half of a rejected batch was written, "+
			"leaving a series with a hole nothing downstream can see", n)
	}
}

// A BAR WITH NO KNOWLEDGE TIME IS REFUSED. Without it the bar cannot be read
// point-in-time, which is this store's entire contract — and an unstamped row
// would be either always or never visible to a horizon query depending on the
// zero value, neither of which is an answer.
func TestMemoryBarsRefuseAnUnstampedBar(t *testing.T) {
	m := NewMemory()

	b := bar(0, 10500, 1)
	b.KnowledgeTime = time.Time{}
	if err := m.PutBars(context.Background(), []Bar{b}); !errors.Is(err, ErrInvalidBar) {
		t.Fatalf("error = %v, want ErrInvalidBar — a bar with no knowledge_time was stored", err)
	}
}

// A QUERY THAT DOES NOT NAME A SERIES IS REFUSED RATHER THAN ANSWERED BROADLY.
//
// The dangerous default is the empty venue: silently blending two exchanges'
// candles returns a plausible series that belongs to no book an order could
// execute against. Refusing is the only reading that cannot be mistaken for data.
func TestMemoryBarsRefuseAnUnderspecifiedQuery(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	if err := m.PutBars(ctx, []Bar{bar(0, 10500, 1)}); err != nil {
		t.Fatalf("PutBars: %v", err)
	}

	for _, tc := range []struct {
		name string
		q    BarQuery
	}{
		{"no instrument", BarQuery{Venue: "XBIN", Resolution: Resolution1m}},
		{"no venue", BarQuery{InstrumentID: "BTC-USDT", Resolution: Resolution1m}},
		{"no resolution", BarQuery{InstrumentID: "BTC-USDT", Venue: "XBIN"}},
		{"unknown resolution", BarQuery{InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: "47s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.Bars(ctx, tc.q)
			if err == nil {
				t.Fatalf("query with %s was answered with %d bars instead of refused", tc.name, len(got))
			}
		})
	}
}

// AN EMPTY SERIES AND A MISSING ONE ARE THE SAME ANSWER: no bars, no error.
// "Nothing configured" and "checked, and fine" must not look the same at the
// point of DECISION, but at the point of READING an instrument that has not
// traded in the window is a legitimate empty result, not a failure.
func TestMemoryBarsOnAnUnknownSeriesIsEmptyNotAnError(t *testing.T) {
	m := NewMemory()
	got, err := m.Bars(context.Background(), BarQuery{
		InstrumentID: "ETH-USDT", Venue: "XBIN", Resolution: Resolution1m,
	})
	if err != nil {
		t.Fatalf("Bars on an unknown series: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("bars = %d, want 0", len(got))
	}
}
