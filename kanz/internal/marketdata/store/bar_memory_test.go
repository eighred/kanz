package store

import (
	"context"
	"testing"
	"time"
)

// THE MEMORY STORE MUST NOT HAVE A LAXER CONTRACT THAN POSTGRES.
//
// It is what most of the platform's tests run against, so a memory store that
// accepted what the database refuses — or that answered a point-in-time read
// with today's value — would mean the suite proves a store the deployment does
// not implement. These mirror the Postgres assertions deliberately.
func TestMemoryBarsAreBitemporal(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	original := bar(0, 10500, 1)
	restated := bar(0, 10900, 30)
	if err := m.PutBars(ctx, []Bar{original, restated}); err != nil {
		t.Fatalf("PutBars: %v", err)
	}
	// BOTH VERSIONS ARE HELD. A restatement coexists with the original rather
	// than replacing it — the property that makes an as-of read possible at all.
	if n := m.barCount(); n != 2 {
		t.Fatalf("stored versions = %d, want 2 — a restatement overwrote the original, and no "+
			"as-of read can recover what was known before it", n)
	}

	early, err := m.Bars(ctx, BarQuery{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m,
		AsOf: barT0.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(early) != 1 || early[0].Close.GetCoefficient() != 10500 {
		t.Fatalf("as-of T+5m close = %v, want 10500 — a correction from T+30m leaked backwards", early)
	}

	latest, err := m.Bars(ctx, BarQuery{InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: Resolution1m})
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(latest) != 1 || latest[0].Close.GetCoefficient() != 10900 {
		t.Fatalf("latest close = %v, want 10900", latest)
	}
}

// An exact re-put is idempotent here too: redelivery is normal, and a doubled
// bar is a corrupted series.
func TestMemoryBarsPutIsIdempotent(t *testing.T) {
	m := NewMemory()
	b := bar(0, 10500, 1)
	for i := 0; i < 3; i++ {
		if err := m.PutBars(context.Background(), []Bar{b}); err != nil {
			t.Fatalf("PutBars #%d: %v", i, err)
		}
	}
	if n := m.barCount(); n != 1 {
		t.Fatalf("stored versions = %d, want 1", n)
	}
}

// The same validation Postgres applies, applied here — otherwise a test suite
// running against memory would accept candles the database rejects.
func TestMemoryBarsRefuseAnImpossibleCandle(t *testing.T) {
	m := NewMemory()
	bad := bar(0, 10500, 1)
	bad.Low = d(12000, -2) // low above high

	if err := m.PutBars(context.Background(), []Bar{bad}); err == nil {
		t.Fatal("the memory store accepted a candle with low above high, which Postgres refuses")
	}
	if n := m.barCount(); n != 0 {
		t.Fatalf("stored versions = %d, want 0", n)
	}
}

// A series is per (instrument, venue, resolution) here as it is in SQL.
func TestMemoryBarsAreSeparatedByVenue(t *testing.T) {
	m := NewMemory()
	okx := bar(0, 10600, 1)
	okx.Venue = "XOKX"
	if err := m.PutBars(context.Background(), []Bar{bar(0, 10500, 1), okx}); err != nil {
		t.Fatalf("PutBars: %v", err)
	}
	got, err := m.Bars(context.Background(), BarQuery{
		InstrumentID: "BTC-USDT", Venue: "XOKX", Resolution: Resolution1m,
	})
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(got) != 1 || got[0].Close.GetCoefficient() != 10600 {
		t.Fatalf("XOKX series = %v, want only its own candle", got)
	}
}
