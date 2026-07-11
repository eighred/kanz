package trades

import (
	"math/big"
	"testing"
	"time"
)

func at(base time.Time, offset time.Duration) time.Time { return base.Add(offset) }

func tr(price, size string, side Side, ts time.Time) Trade {
	p, _ := new(big.Rat).SetString(price)
	s, _ := new(big.Rat).SetString(size)
	return Trade{Price: p, Size: s, TakerSide: side, EventTime: ts}
}

// The tape aggregates aggressive buy and sell volume separately and exactly.
func TestTape_VolumesByAggressor(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	tape := New("BTC-USD", "BINANCE", time.Minute)
	tape.Add(tr("50000", "1.5", SideBuy, at(t0, 0)))
	tape.Add(tr("50001", "0.5", SideSell, at(t0, time.Second)))
	tape.Add(tr("50002", "2", SideBuy, at(t0, 2*time.Second)))

	buy, sell := tape.Volumes(time.Minute)
	if want := new(big.Rat).SetFrac64(35, 10); buy.Cmp(want) != 0 { // 1.5 + 2
		t.Fatalf("buy volume = %s, want 3.5", buy.RatString())
	}
	if want := new(big.Rat).SetFrac64(5, 10); sell.Cmp(want) != 0 {
		t.Fatalf("sell volume = %s, want 0.5", sell.RatString())
	}
	if got := tape.LastPrice(); got == nil || got.Cmp(big.NewRat(50002, 1)) != 0 {
		t.Fatalf("last price = %v, want 50002", got)
	}
}

// A trade whose aggressor the venue did not report contributes to NEITHER side.
// Attributing it to one would fabricate directional pressure that never existed.
func TestTape_UnknownAggressorCountsToNeitherSide(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	tape := New("BTC-USD", "OKX", time.Minute)
	tape.Add(tr("50000", "1", SideBuy, at(t0, 0)))
	tape.Add(tr("50000", "99", SideUnknown, at(t0, time.Second)))

	buy, sell := tape.Volumes(time.Minute)
	if buy.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("buy = %s, want exactly 1 (the unknown-aggressor trade must not count)", buy.RatString())
	}
	if sell.Sign() != 0 {
		t.Fatalf("sell = %s, want 0", sell.RatString())
	}
}

// The window is measured from the newest trade, and volume outside it is excluded.
func TestTape_VolumesWindowed(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	tape := New("BTC-USD", "BINANCE", time.Hour)
	tape.Add(tr("50000", "10", SideBuy, at(t0, 0)))             // 30s before the last
	tape.Add(tr("50000", "1", SideBuy, at(t0, 30*time.Second))) // the newest

	buy, _ := tape.Volumes(10 * time.Second) // only the newest is inside
	if buy.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("windowed buy = %s, want 1 (the older 10 is outside the window)", buy.RatString())
	}
	// A window wider than the tape's retention is clamped, never extrapolated.
	buy, _ = tape.Volumes(999 * time.Hour)
	if buy.Cmp(big.NewRat(11, 1)) != 0 {
		t.Fatalf("clamped buy = %s, want 11 (all retained volume)", buy.RatString())
	}
}

// The tape is bounded by its retention window: an unbounded tape on a busy
// instrument is a memory leak in a long-lived edge process.
func TestTape_PrunesPastRetention(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	tape := New("BTC-USD", "BINANCE", 10*time.Second)
	tape.Add(tr("50000", "1", SideBuy, at(t0, 0)))
	tape.Add(tr("50000", "1", SideBuy, at(t0, 5*time.Second)))
	if tape.Len() != 2 {
		t.Fatalf("len = %d, want 2 (both within retention)", tape.Len())
	}
	// This trade is 60s past the first, which falls outside the 10s retention.
	tape.Add(tr("50000", "1", SideBuy, at(t0, 60*time.Second)))
	if tape.Len() != 1 {
		t.Fatalf("len = %d, want 1 (the stale trades must be pruned)", tape.Len())
	}
}

// A zero-size trade is noise, not an execution.
func TestTape_ZeroSizeIgnored(t *testing.T) {
	tape := New("BTC-USD", "BINANCE", time.Minute)
	tape.Add(tr("50000", "0", SideBuy, time.Unix(1_700_000_000, 0).UTC()))
	if tape.Len() != 0 {
		t.Fatal("a zero-size trade must not enter the tape")
	}
}
