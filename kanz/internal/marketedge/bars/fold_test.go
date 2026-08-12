package bars

import (
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketedge/trades"
)

var t0 = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

func btc() Series { return Series{InstrumentID: "BTC-USDT", Venue: "XBIN"} }

func tr(price, size int64, sec int) trades.Trade {
	return trades.Trade{
		Price:     big.NewRat(price, 1),
		Size:      big.NewRat(size, 1),
		EventTime: t0.Add(time.Duration(sec) * time.Second),
	}
}

func rat(v *big.Rat) string {
	if v == nil {
		return "<nil>"
	}
	return v.RatString()
}

// THE CANDLE IS THE FOUR PRICES AND THE VOLUME, and getting OHLC right is the
// entire job: the fill simulator (#428) keys on Low and High to decide whether a
// limit order would have been reachable, so a wrong high is a fill that never
// happened.
func TestAFoldedCandleCarriesOHLCV(t *testing.T) {
	f := New()
	// 100 (open), 130 (high), 90 (low), 110 (close) — deliberately not monotonic,
	// so a fold that just tracked first/last would pass while high/low were wrong.
	for _, x := range []trades.Trade{tr(100, 1, 0), tr(130, 2, 10), tr(90, 3, 20), tr(110, 4, 30)} {
		if closed, _ := f.Add(btc(), x); closed != nil {
			t.Fatalf("a trade inside the same minute closed a candle: %+v", closed)
		}
	}
	// A trade in the NEXT minute is what closes it.
	closed, late := f.Add(btc(), tr(200, 1, 60))
	if late {
		t.Fatal("a later trade was reported as out-of-order")
	}
	if closed == nil {
		t.Fatal("crossing the minute boundary closed no candle")
	}
	for _, c := range []struct {
		name string
		got  *big.Rat
		want string
	}{
		{"open", closed.Open, "100"}, {"high", closed.High, "130"},
		{"low", closed.Low, "90"}, {"close", closed.Close, "110"},
		{"volume", closed.Volume, "10"},
	} {
		if rat(c.got) != c.want {
			t.Errorf("%s = %s, want %s", c.name, rat(c.got), c.want)
		}
	}
	if closed.TradeCount != 4 {
		t.Errorf("trade_count = %d, want 4", closed.TradeCount)
	}
	if !closed.BucketStart.Equal(t0) {
		t.Errorf("bucket_start = %s, want %s", closed.BucketStart, t0)
	}
}

// THE BOUNDARY COMES FROM THE DATA, NOT A CLOCK. A replay of the same trades
// must produce the same candles as the live run did — which is the property the
// backtest depends on, and it is only true if nothing here reads wall time.
func TestTheBoundaryIsDiscoveredFromTheTrades(t *testing.T) {
	f := New()
	f.Add(btc(), tr(100, 1, 0))
	closed, _ := f.Add(btc(), tr(200, 1, 59)) // still inside minute 0
	if closed != nil {
		t.Fatal("a trade at :59 closed the minute it belongs to")
	}
	closed, _ = f.Add(btc(), tr(300, 1, 60)) // minute 1
	if closed == nil {
		t.Fatal("the first trade of the next minute closed nothing")
	}
	if rat(closed.Close) != "200" {
		t.Errorf("close = %s, want 200 — the :59 trade belongs to the closed candle", rat(closed.Close))
	}
}

// SERIES DO NOT MIX. A bar from one venue is not a bar from another: different
// books, different prints, a different close for the same minute. Folding them
// together would produce a candle no market traded.
func TestSeriesAreFoldedSeparately(t *testing.T) {
	f := New()
	okx := Series{InstrumentID: "BTC-USDT", Venue: "XOKX"}

	f.Add(btc(), tr(100, 1, 0))
	f.Add(okx, tr(500, 1, 0))
	f.Add(btc(), tr(110, 1, 10))

	out := f.Flush(t0.Add(2 * time.Minute))
	if len(out) != 2 {
		t.Fatalf("flushed %d candles, want 2 (one per venue)", len(out))
	}
	got := map[string]string{}
	for _, b := range out {
		got[b.Series.Venue] = rat(b.Close)
	}
	if got["XBIN"] != "110" || got["XOKX"] != "500" {
		t.Errorf("closes = %v, want XBIN 110 and XOKX 500 — the two venues were folded together", got)
	}
}

// A QUIET SERIES STILL CLOSES. Add only discovers a boundary when a later trade
// arrives, so without Flush the last candle of a lull sits open forever — and on
// a thin instrument a lull is the normal case, not an edge one.
func TestFlushClosesACandleNoLaterTradeWouldHaveClosed(t *testing.T) {
	f := New()
	f.Add(btc(), tr(100, 1, 0))

	out := f.Flush(t0.Add(90 * time.Second))
	if len(out) != 1 {
		t.Fatalf("flushed %d candles, want 1", len(out))
	}
	if rat(out[0].Close) != "100" {
		t.Errorf("close = %s, want 100", rat(out[0].Close))
	}
	// And it is gone: flushing again must not republish it.
	if again := f.Flush(t0.Add(5 * time.Minute)); len(again) != 0 {
		t.Fatalf("a flushed candle was emitted again: %+v", again)
	}
}

// THE MINUTE IN PROGRESS IS NOT PUBLISHED. Its candle is incomplete, and a
// partial bar in a store whose contract is "this is what happened" is a lie the
// next reader cannot detect.
func TestFlushDoesNotCloseTheMinuteInProgress(t *testing.T) {
	f := New()
	f.Add(btc(), tr(100, 1, 0))

	if out := f.Flush(t0.Add(30 * time.Second)); len(out) != 0 {
		t.Fatalf("flushed the candle for the minute still in progress: %+v", out)
	}
}

// A MINUTE WITH NO TRADES PRODUCES NO BAR. This process cannot tell "nothing
// traded" from "the feed was down" — a fabricated flat candle asserts the first,
// and an indicator would compute a real number from a claim nobody checked.
func TestAQuietMinuteProducesNoCandle(t *testing.T) {
	f := New()
	f.Add(btc(), tr(100, 1, 0))
	f.Flush(t0.Add(2 * time.Minute)) // closes minute 0

	// Minutes 1..4 saw nothing at all.
	if out := f.Flush(t0.Add(5 * time.Minute)); len(out) != 0 {
		t.Fatalf("invented %d candle(s) for minutes that had no trades: %+v", len(out), out)
	}
}

// AN OUT-OF-ORDER PRINT IS DROPPED AND REPORTED, never backdated. Folding it
// would restate a candle the caller may already have published, and a silent
// restatement is exactly what the bitemporal store exists to make impossible.
func TestALatePrintIsDroppedAndReported(t *testing.T) {
	f := New()
	f.Add(btc(), tr(100, 1, 0))
	if closed, _ := f.Add(btc(), tr(200, 1, 60)); closed == nil {
		t.Fatal("the boundary trade closed nothing")
	}

	closed, late := f.Add(btc(), tr(999, 1, 30)) // belongs to the closed minute
	if !late {
		t.Fatal("a print for an already-closed minute was accepted silently")
	}
	if closed != nil {
		t.Fatalf("a late print closed a candle: %+v", closed)
	}
	// It must not have polluted the open candle either.
	out := f.Flush(t0.Add(3 * time.Minute))
	if len(out) != 1 || rat(out[0].High) != "200" {
		t.Fatalf("the late print leaked into the open candle: %+v", out)
	}
}

// Noise is ignored on the same terms trades.Tape ignores it: a zero-size print
// is not an execution, and a non-positive price is not a price.
func TestNoiseIsIgnored(t *testing.T) {
	f := New()
	for _, bad := range []trades.Trade{
		{Price: big.NewRat(100, 1), Size: big.NewRat(0, 1), EventTime: t0},
		{Price: big.NewRat(0, 1), Size: big.NewRat(1, 1), EventTime: t0},
		{Price: nil, Size: big.NewRat(1, 1), EventTime: t0},
		{Price: big.NewRat(100, 1), Size: nil, EventTime: t0},
	} {
		if closed, late := f.Add(btc(), bad); closed != nil || late {
			t.Fatalf("noise produced a result: %+v late=%v", closed, late)
		}
	}
	if out := f.Flush(t0.Add(2 * time.Minute)); len(out) != 0 {
		t.Fatalf("noise alone produced a candle: %+v", out)
	}
}

// The fold keeps its own copies. A caller reusing a Trade struct, or a tape
// pruning underneath it, must not be able to change a candle already folded —
// big.Rat is a pointer type and sharing one would do exactly that.
func TestTheFoldCopiesItsInputs(t *testing.T) {
	f := New()
	price := big.NewRat(100, 1)
	f.Add(btc(), trades.Trade{Price: price, Size: big.NewRat(1, 1), EventTime: t0})

	price.SetInt64(9999) // the caller reuses its Rat

	out := f.Flush(t0.Add(2 * time.Minute))
	if len(out) != 1 || rat(out[0].Open) != "100" {
		t.Fatalf("mutating the caller's Rat changed a folded candle: %+v", out)
	}
}
