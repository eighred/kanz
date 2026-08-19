package indicator

import (
	"context"
	"math"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
)

var covOrigin = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func covDec(f float64) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: int64(math.Round(f * 100)), Exponent: -2}
}

// gappedBars puts one bar every `spacing` minutes: a 1m series in which
// spacing-1 of every spacing minutes is absent.
func gappedBars(n int, spacing int) []store.Bar {
	out := make([]store.Bar, 0, n)
	for i := 0; i < n; i++ {
		start := covOrigin.Add(time.Duration(i*spacing) * time.Minute)
		px := 100.0 + float64(i)
		out = append(out, store.Bar{
			InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: store.Resolution1m,
			BucketStart: start,
			Open:        covDec(px), High: covDec(px), Low: covDec(px), Close: covDec(px),
			Volume: covDec(1), KnowledgeTime: start.Add(time.Minute),
		})
	}
	return out
}

// A PERIOD IS A DURATION, NOT AN ELEMENT COUNT (#416).
//
// Measured before the fix, over exactly this fixture: rsi_14 came back as 100 —
// "maximally overbought" — computed from fifteen prints spread across 150
// minutes and presented as a fourteen-minute reading. atr_14 answered too. Both
// were in range, both plausible, and both answering a question nobody asked.
func TestAGappedSeriesYieldsNoPeriodReadings(t *testing.T) {
	m := store.NewMemory()
	if err := m.PutBars(context.Background(), gappedBars(25, 10)); err != nil {
		t.Fatal(err)
	}
	src, err := NewSource(m, Config{Venue: "XBIN"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := src.FeaturesAsOf(context.Background(), "BTC-USDT", covOrigin.Add(300*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"sma_20", "sma_50", "ema_12", "ema_26", "rsi_14",
		"realized_vol_20", "atr_14", "macd", "bb_middle"} {
		if v, present := f[k]; present {
			t.Errorf("%s = %v over a series with 9 of every 10 minutes missing — the contiguous "+
				"tail is one bar long, so no period fits", k, v)
		}
	}
}

// THE UNBROKEN CASE IS UNCHANGED, which is what stops the fix above from being
// "return nothing, always". Without this a reader cannot tell a careful trim
// from a blanket refusal, and every indicator in the package would be dead.
func TestAnUnbrokenSeriesStillReports(t *testing.T) {
	m := store.NewMemory()
	if err := m.PutBars(context.Background(), gappedBars(60, 1)); err != nil {
		t.Fatal(err)
	}
	src, err := NewSource(m, Config{Venue: "XBIN"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := src.FeaturesAsOf(context.Background(), "BTC-USDT", covOrigin.Add(60*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"sma_20", "ema_12", "rsi_14", "atr_14", "macd"} {
		if _, present := f[k]; !present {
			t.Errorf("%s is absent over an unbroken 60-bar series — the trim is refusing "+
				"readings it can vouch for", k)
		}
	}
}

// ONLY THE TAIL AFTER THE LAST HOLE IS READ. A series that was broken an hour ago
// and has run cleanly since must still produce readings: refusing it would blank
// every instrument that has ever been quiet, which is all of them.
func TestReadingsResumeAfterTheHole(t *testing.T) {
	m := store.NewMemory()
	// A clean run of 40 minutes, a hole, then a clean run of 40 minutes.
	var bars []store.Bar
	bars = append(bars, gappedBars(40, 1)...)
	for i := 0; i < 40; i++ {
		start := covOrigin.Add(time.Duration(100+i) * time.Minute)
		px := 200.0 + float64(i)
		bars = append(bars, store.Bar{
			InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: store.Resolution1m,
			BucketStart: start,
			Open:        covDec(px), High: covDec(px), Low: covDec(px), Close: covDec(px),
			Volume: covDec(1), KnowledgeTime: start.Add(time.Minute),
		})
	}
	if err := m.PutBars(context.Background(), bars); err != nil {
		t.Fatal(err)
	}
	src, err := NewSource(m, Config{Venue: "XBIN"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := src.FeaturesAsOf(context.Background(), "BTC-USDT", covOrigin.Add(140*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, present := f["sma_20"]; !present {
		t.Error("sma_20 absent although the most recent 40 minutes are unbroken")
	}
	// 40 contiguous bars cannot support a 50-period average.
	if v, present := f["sma_50"]; present {
		t.Errorf("sma_50 = %v: the contiguous tail is 40 bars, and the 10 bars before the hole "+
			"are not adjacent to it", v)
	}
}
