package trades

import (
	"math/big"
	"runtime"
	"testing"
	"time"
)

// THE TAPE RELEASES WHAT IT PRUNES (#880).
//
// # Why a finalizer and not a length assertion
//
// Every retention test in this package counts, and a count cannot see
// reachability.
//
// THIS GUARDS THE NEW SHAPE, not the old one, and the distinction is worth
// stating because #880 got it the other way round. The old prune compacted with
// `append(t.trades[:0], t.trades[i:]...)`, which OVERWRITES as it copies — the
// slots it left past len held duplicates of still-live elements, and the next
// append wrote over them. It was O(n) per print, which is why it is gone, but it
// was not leaking.
//
// A RESLICE OVERWRITES NOTHING. pit.DropOldest returns xs[drop:], and Go keeps an
// entire array alive while any slice references any part of it, so the dropped
// trades — and each one's *big.Rat price and size — stay reachable behind the
// returned slice unless something clears them. That is #862 exactly, and this
// tape has just inherited it by adopting the faster shape.
//
// So the clear is load-bearing HERE in a way it never was before, and nothing
// else in this package can tell whether it happened: len(t.trades) is right
// either way.
//
// So this attaches a finalizer to the price the prune is about to drop, forces a
// GC, and requires the finalizer to run: a direct statement about the heap
// rather than about a slice header. Without it the release is unfalsifiable —
// the clear can be deleted and every other assertion here still passes.

// tradeAt builds one trade whose Price carries the identity under test.
func tradeAt(price *big.Rat, at time.Time) Trade {
	return Trade{
		Price:     price,
		Size:      big.NewRat(1, 1),
		EventTime: at,
	}
}

// THE SHAPE OF THIS TEST IS LOAD-BEARING, and the first version of it was
// VACUOUS — it passed with the clear deleted, which the mutation harness caught.
//
// The reason is that a tape under load hides the defect by accident. Every Add
// appends, the slice has been resliced forward so its spare capacity shrinks, and
// once an append exceeds cap Go allocates a NEW array and copies only the live
// elements across. The old array — stale head and all — then becomes garbage
// wholesale, and the finalizer runs whether or not anything was ever cleared.
//
// So the prune must happen ONCE, with capacity still to spare, and NOTHING may be
// appended afterwards. Then the only thing that can release the dropped trade is
// the clear itself.
func TestThePrunedTradeIsReleased(t *testing.T) {
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	tape := New("BTC-USD", "BINANCE", time.Minute)

	gone := make(chan struct{})
	func() {
		first := big.NewRat(50000, 1)
		runtime.SetFinalizer(first, func(*big.Rat) { close(gone) })
		tape.Add(tradeAt(first, base))
		// first goes out of scope here; the tape holds the only reference.
	}()

	// Fill WITHIN the window so nothing is pruned yet and the slice grows itself
	// some spare capacity.
	for i := 1; i < 32; i++ {
		tape.Add(tradeAt(big.NewRat(int64(50000+i), 1), base.Add(time.Duration(i)*time.Second)))
	}
	if got := tape.Len(); got != 32 {
		t.Fatalf("tape holds %d trades, want 32 — the fill pruned something and the single-prune "+
			"shape this test depends on is gone", got)
	}

	// ONE trade past the horizon: this prunes, and it is the last append. Anything
	// appended after it could reallocate and release the old array for the wrong
	// reason.
	tape.Add(tradeAt(big.NewRat(60000, 1), base.Add(2*time.Minute)))

	// NON-VACUITY: the prune must actually have dropped the first trade, or the
	// finalizer arm below proves nothing. Counting is the right check HERE
	// precisely because it is the half that already worked.
	if got := tape.Len(); got != 1 {
		t.Fatalf("tape holds %d trades after a print two minutes past a one-minute window, want 1 — "+
			"nothing was pruned, so this test would pass without testing release", got)
	}

	// A dropped element becomes unreachable only after a collection, and one GC is
	// not guaranteed to run every finalizer.
	//
	// runtime.KeepAlive(tape) IS THE WHOLE TEST. Without it this passes with the
	// clear deleted — caught by the mutation harness — because `tape` is dead
	// after its last use above, so the collector takes the Tape, its slice and the
	// backing array together and the finalizer runs for a reason that has nothing
	// to do with pruning. KeepAlive placed AFTER the loop keeps the tape reachable
	// for the whole of it, on both the success and failure paths, so the only
	// thing that can release the dropped trade is the clear.
	released := false
	for i := 0; i < 40 && !released; i++ {
		runtime.GC()
		select {
		case <-gone:
			released = true
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	runtime.KeepAlive(tape)
	if released {
		return
	}
	t.Fatal("the pruned trade's price was never collected: it is out of the tape by every count and " +
		"still reachable through the backing array, because a reslice overwrites nothing. That is " +
		"#862's defect, inherited here by adopting the faster prune — a tape that reports the right " +
		"length while holding trades it has dropped, on the market-data ingest path. The prune must " +
		"RELEASE what it drops (#880).")
}

// BenchmarkAddByTapeDepth measures what one print costs as the tape fills.
//
// The prune used to copy every live element down on every trade, so the cost per
// print grew with the tape's depth — O(n) per trade under the write lock. It
// should now be flat: pit.DropOldest is a reslice plus an O(drop) clear, and drop
// is 1 in the steady state.
func BenchmarkAddByTapeDepth(b *testing.B) {
	for _, n := range []int{100, 1000, 10000, 100000} {
		b.Run(benchName(n), func(b *testing.B) {
			base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
			// A retention window wide enough that the seeded trades all stay live,
			// so each Add below prunes at most one and pays the depth cost.
			tape := New("BTC-USD", "BINANCE", time.Duration(n+10)*time.Millisecond)
			for i := 0; i < n; i++ {
				tape.Add(tradeAt(big.NewRat(int64(50000+i%100), 1), base.Add(time.Duration(i)*time.Millisecond)))
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tape.Add(tradeAt(big.NewRat(50001, 1), base.Add(time.Duration(n+i)*time.Millisecond)))
			}
		})
	}
}

func benchName(n int) string {
	switch {
	case n >= 100000:
		return "depth=100000"
	case n >= 10000:
		return "depth=10000"
	case n >= 1000:
		return "depth=1000"
	default:
		return "depth=100"
	}
}
