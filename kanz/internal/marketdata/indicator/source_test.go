package indicator

import (
	"context"
	"math"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/marketdata/store"
)

var origin = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func dec(f float64) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: int64(math.Round(f * 100)), Exponent: -2}
}

// bar builds one 1-minute candle for BTC on XBIN at minute i.
func bar(i int, close float64, known time.Time) store.Bar {
	return store.Bar{
		InstrumentID:  "BTC-USDT",
		Venue:         "XBIN",
		Resolution:    store.Resolution1m,
		BucketStart:   origin.Add(time.Duration(i) * time.Minute),
		Open:          dec(close),
		High:          dec(close + 1.5),
		Low:           dec(close - 1.5),
		Close:         dec(close),
		Volume:        dec(10),
		KnowledgeTime: known,
	}
}

// seed writes n ascending bars, all known at their own bucket time.
func seed(t *testing.T, n int) *store.Memory {
	t.Helper()
	m := store.NewMemory()
	bars := make([]store.Bar, 0, n)
	for i := 0; i < n; i++ {
		b := bar(i, 100+float64(i), origin.Add(time.Duration(i)*time.Minute))
		bars = append(bars, b)
	}
	if err := m.PutBars(context.Background(), bars); err != nil {
		t.Fatal(err)
	}
	return m
}

func newSource(t *testing.T, m store.BarStore) *Source {
	t.Helper()
	s, err := NewSource(m, Config{Venue: "XBIN"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A CORRECTION THAT ARRIVED AFTER asOf IS INVISIBLE.
//
// The property the whole point-in-time contract exists for, and the one that
// cannot be checked by looking at the numbers: a leaking implementation returns
// perfectly plausible indicators computed from prices nobody had yet. So it is
// checked by CHANGING the answer and proving the earlier read did not move.
func TestFeaturesAsOf_ACorrectionStampedLaterCannotLeakBackwards(t *testing.T) {
	m := seed(t, 60)
	ctx := context.Background()
	asOf := origin.Add(60 * time.Minute)

	before, err := newSource(t, m).FeaturesAsOf(ctx, "BTC-USDT", asOf)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := before["sma_20"]; !ok {
		t.Fatal("no sma_20 over 60 bars — the fixture is not reaching the indicator")
	}

	// A VENUE RESTATES bar 55, drastically, and Kanz learns it an hour after asOf.
	restated := bar(55, 100_000, asOf.Add(time.Hour))
	if err := m.PutBars(ctx, []store.Bar{restated}); err != nil {
		t.Fatal(err)
	}

	after, err := newSource(t, m).FeaturesAsOf(ctx, "BTC-USDT", asOf)
	if err != nil {
		t.Fatal(err)
	}
	if after["sma_20"] != before["sma_20"] {
		t.Errorf("sma_20 moved from %v to %v after a correction stamped an hour past asOf — the "+
			"query is reading the knowledge axis unbounded, and every backtest run through it "+
			"reports a strategy that could not have existed",
			before["sma_20"], after["sma_20"])
	}

	// AND THE CORRECTION IS VISIBLE once asOf passes its knowledge time — without
	// this the test above is satisfied by a store that never returns corrections
	// at all, which would look identical and be a different bug.
	later, err := newSource(t, m).FeaturesAsOf(ctx, "BTC-USDT", asOf.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if later["sma_20"] == before["sma_20"] {
		t.Error("the restatement never became visible even well past its knowledge time — the " +
			"no-leak test above would pass on a store that simply drops corrections")
	}
}

// INSUFFICIENT HISTORY YIELDS FEWER KEYS, NEVER ZEROS.
//
// A zero rsi_14 means "sold off hard" and a zero realized_vol_20 means "did not
// move". Both are readings. Neither is "we had 10 bars".
func TestFeaturesAsOf_ShortHistoryOmitsKeysRatherThanZeroingThem(t *testing.T) {
	m := seed(t, 10)
	got, err := newSource(t, m).FeaturesAsOf(context.Background(), "BTC-USDT",
		origin.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"sma_20", "sma_50", "rsi_14", "atr_14", "macd", "bb_middle", "realized_vol_20"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s = %v over 10 bars — an indicator with no answer must be ABSENT, because "+
				"every number it could return is also a real reading", k, v)
		}
	}
	// ema_12 and sma_20 both need more than 10; nothing at all should be
	// computable, and an empty map is the honest answer.
	if len(got) != 0 {
		t.Errorf("got %d features over 10 bars: %v", len(got), got)
	}
}

// THE FULL SET APPEARS ONCE THE HISTORY IS THERE.
//
// Without this, the test above is satisfied by a Source that always returns
// nothing — which is the failure mode that looks exactly like a quiet market.
func TestFeaturesAsOf_AFullHistoryProducesEveryReading(t *testing.T) {
	m := seed(t, 120)
	got, err := newSource(t, m).FeaturesAsOf(context.Background(), "BTC-USDT",
		origin.Add(120*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"sma_20", "sma_50", "ema_12", "ema_26", "rsi_14", "realized_vol_20", "atr_14",
		"macd", "macd_signal", "macd_histogram",
		"bb_middle", "bb_upper", "bb_lower", "bb_width",
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("%s missing over 120 bars", k)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d features, want %d — a key added without a line in this list is a "+
			"feature no trainer knows to expect: %v", len(got), len(want), got)
	}
	// A MONOTONICALLY RISING SERIES: RSI pinned at 100, and the fast EMA above the
	// slow one. Sanity that the readings describe THIS series rather than any.
	if got["rsi_14"] != 100 {
		t.Errorf("rsi_14 = %v on a strictly rising series, want 100", got["rsi_14"])
	}
	if got["macd"] <= 0 {
		t.Errorf("macd = %v on a strictly rising series, want positive", got["macd"])
	}
}

// ANOTHER VENUE'S BARS DO NOT LEAK IN.
//
// store.Bar's own doc makes venue part of a bar's identity: a composite candle is
// a price nobody can trade at, and a model trained on one diverges live in a way
// that reads as "the strategy stopped working".
func TestFeaturesAsOf_AnotherVenueIsADifferentSeries(t *testing.T) {
	m := seed(t, 120)
	ctx := context.Background()

	// The SAME instrument on a different venue, at wildly different prices.
	other := make([]store.Bar, 0, 120)
	for i := 0; i < 120; i++ {
		b := bar(i, 500+float64(i), origin.Add(time.Duration(i)*time.Minute))
		b.Venue = "XOKX"
		other = append(other, b)
	}
	if err := m.PutBars(ctx, other); err != nil {
		t.Fatal(err)
	}

	got, err := newSource(t, m).FeaturesAsOf(ctx, "BTC-USDT", origin.Add(120*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// XBIN's closes run 100..219; the SMA(20) must sit in that range, not in a
	// blend with XOKX's 500..619.
	if got["sma_20"] < 100 || got["sma_20"] > 220 {
		t.Errorf("sma_20 = %v — outside the XBIN series, so the other venue's candles were "+
			"averaged in and the result is a price nobody can trade at", got["sma_20"])
	}
}

// A SOURCE WITH NO VENUE IS REFUSED AT CONSTRUCTION.
//
// Reading "every venue" would silently build the composite series the platform
// does not have, so it is refused where it can be seen rather than producing a
// blended number at query time.
func TestNewSource_RefusesAnEmptyVenue(t *testing.T) {
	if _, err := NewSource(store.NewMemory(), Config{}); err == nil {
		t.Fatal("a Source with no venue was constructed")
	}
	if _, err := NewSource(nil, Config{Venue: "XBIN"}); err == nil {
		t.Fatal("a Source with no store was constructed")
	}
	if _, err := NewSource(store.NewMemory(), Config{Venue: "XBIN", Resolution: "7m"}); err == nil {
		t.Fatal("a Source with an unstored resolution was constructed")
	}
	// The default is the BASE series, not a rollup.
	s, err := NewSource(store.NewMemory(), Config{Venue: "XBIN"})
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.Resolution != store.Resolution1m {
		t.Errorf("default resolution = %q, want 1m", s.cfg.Resolution)
	}
	if s.cfg.Lookback != DefaultLookback {
		t.Errorf("default lookback = %d, want %d", s.cfg.Lookback, DefaultLookback)
	}
}

// A NIL PRICE YIELDS NO READINGS AT ALL, not readings computed around the hole.
//
// A series with holes silently shortens every window: a 20-period SMA would
// average 20 values spanning 25 minutes and report as though it spanned 20.
func TestFeatures_ANilPriceProducesNothing(t *testing.T) {
	bars := make([]store.Bar, 0, 60)
	for i := 0; i < 60; i++ {
		bars = append(bars, bar(i, 100+float64(i), origin))
	}
	bars[30].Close = nil

	if got := Features(bars); len(got) != 0 {
		t.Errorf("got %d features from a series with a nil close: %v — a partial series must "+
			"produce no reading rather than one computed around the hole", len(got), got)
	}
}

// Features AND FeaturesAsOf AGREE, because they are the same function.
//
// The readings a strategy is backtested on and the readings it trades on must
// come from one implementation. If they ever diverge, a live loss gets blamed on
// the strategy and the cause is in the plumbing.
func TestFeatures_TheInMemoryAndStorePathsAgree(t *testing.T) {
	m := seed(t, 120)
	ctx := context.Background()
	asOf := origin.Add(120 * time.Minute)

	viaStore, err := newSource(t, m).FeaturesAsOf(ctx, "BTC-USDT", asOf)
	if err != nil {
		t.Fatal(err)
	}
	bars, err := m.Bars(ctx, store.BarQuery{
		InstrumentID: "BTC-USDT", Venue: "XBIN", Resolution: store.Resolution1m,
		From: asOf.Add(-DefaultLookback * time.Minute), To: asOf, AsOf: asOf,
	})
	if err != nil {
		t.Fatal(err)
	}
	direct := Features(bars)

	if len(direct) != len(viaStore) {
		t.Fatalf("%d features in memory, %d through the store", len(direct), len(viaStore))
	}
	for k, v := range direct {
		if viaStore[k] != v {
			t.Errorf("%s: in-memory %v, through the store %v", k, v, viaStore[k])
		}
	}
}
