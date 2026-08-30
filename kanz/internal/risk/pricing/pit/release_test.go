package pit

import (
	"runtime"
	"testing"
	"time"
)

// THE HORIZON RELEASES WHAT IT DROPS (#862).
//
// # Why a finalizer and not a length assertion
//
// Every other retention test in this tree asserts on len(vs) or on what At
// resolves. Both were already correct while the defect was live, and that is the
// whole point: Put returned vs[drop:], which reslices the SAME backing array, so
// the dropped Version[T] values were gone from the slice and still in memory —
// Go keeps an entire array alive while any slice references any part of it.
//
// A count cannot see that. Reachability can. These tests attach a finalizer to
// the payload a prune is about to drop, force a GC, and require the finalizer to
// run: that is a direct statement about the heap rather than about a header.
//
// Without such a test the fix is unfalsifiable — clear(vs[:drop]) can be deleted
// and every length-based assertion in the package still passes.

// payload is a pointer type so it can carry a finalizer. A real T here is a
// curve or a vol surface; what matters is only that the array's reference to it
// is the last one.
type payload struct{ id int }

// collected drives n one-minute refreshes through Put under horizon, holding a
// finalizer on the FIRST payload — the one the prune must drop — and reports
// whether it became unreachable.
func collected(t *testing.T, horizon time.Duration, n int) bool {
	t.Helper()

	base := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	gone := make(chan struct{})

	var vs []Version[*payload]
	func() {
		first := &payload{id: 0}
		runtime.SetFinalizer(first, func(*payload) { close(gone) })
		vs, _ = Put(vs, base, first, horizon)
		// first goes out of scope here; the only reference left is the store's.
	}()

	for i := 1; i < n; i++ {
		vs, _ = Put(vs, base.Add(time.Duration(i)*time.Minute), &payload{id: i}, horizon)
	}

	// NON-VACUITY: the prune must actually have dropped the first version, or
	// this proves nothing about release. len is the right check here precisely
	// because it is the half that already worked.
	if len(vs) >= n {
		t.Fatalf("after %d refreshes the store still holds %d versions — nothing was pruned, "+
			"so this test is not measuring release", n, len(vs))
	}
	if vs[0].V.id == 0 {
		t.Fatal("the first payload is still the head of the store — it was not dropped")
	}

	// THE STORE MUST STAY LIVE ACROSS THE GC, and this KeepAlive is the whole
	// test. Without it the compiler sees vs is never used again, the entire
	// backing array becomes garbage, and the finalizer fires whatever Put did —
	// the test passes against the defect. Mutation caught exactly that here.
	// A real store is held by its owner for the life of the process; this is that
	// condition, stated.
	out := gcUntil(gone)
	runtime.KeepAlive(vs)
	return out
}

// gcUntil forces collection cycles until done is closed, or gives up.
//
// MORE THAN ONE CYCLE IS REQUIRED, not defensive padding: a finalizer runs on
// its own goroutine no earlier than the cycle after the one that found the
// object unreachable, so a single runtime.GC() proves nothing either way.
func gcUntil(done <-chan struct{}) bool {
	for i := 0; i < 8; i++ {
		runtime.GC()
		select {
		case <-done:
			return true
		case <-time.After(100 * time.Millisecond):
		}
	}
	return false
}

// A DROPPED VERSION IS RELEASED, NOT MERELY UNLINKED.
//
// IT DOES NOT, ON ITS OWN, DISCRIMINATE — and saying so is the point. This starts
// from a nil slice, so append reallocates repeatedly over the run and each
// reallocation copies only the live elements, dropping the first payload whatever
// the prune did. Under the defect it passes. It is here because it states the
// ordinary case and would catch a prune that stopped pruning at all;
// TestPruneDoesNotWaitForAReallocation below is the arm that fails without
// clear(), and it is the reason this file exists.
func TestPrunedVersionBecomesUnreachable(t *testing.T) {
	// A 30-minute horizon over 240 one-minute refreshes: the first version is far
	// outside it and must be both dropped and released.
	if !collected(t, 30*time.Minute, 240) {
		t.Fatal("a version dropped by the horizon was never collected — Put reslices the " +
			"backing array and the pruned payloads are still reachable from it, so the " +
			"horizon bounds the version COUNT while the heap keeps growing. On the risk " +
			"engine that is roughly double the intended peak for every pricing key")
	}
}

// AND IT IS RELEASED WHILE THE STORE IS STILL SMALL, not only when a later
// append happens to reallocate.
//
// This is the arm that distinguishes the fix from the accident: with a large
// initial capacity the array is never replaced during the run, so an
// implementation relying on reallocation to free the prefix fails here while
// passing the test above.
func TestPruneDoesNotWaitForAReallocation(t *testing.T) {
	base := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	gone := make(chan struct{})

	// Capacity for the whole run up front: append never needs to grow, so nothing
	// but the prune itself can release the dropped payload.
	vs := make([]Version[*payload], 0, 512)
	func() {
		first := &payload{id: 0}
		runtime.SetFinalizer(first, func(*payload) { close(gone) })
		vs, _ = Put(vs, base, first, 10*time.Minute)
	}()
	for i := 1; i < 200; i++ {
		vs, _ = Put(vs, base.Add(time.Duration(i)*time.Minute), &payload{id: i}, 10*time.Minute)
	}
	if cap(vs) > 512 {
		t.Fatalf("the backing array was reallocated (cap %d) — this test needs it not to be, "+
			"or it cannot tell the prune apart from the reallocation", cap(vs))
	}

	released := gcUntil(gone)
	runtime.KeepAlive(vs) // see collected: without this the array itself is garbage
	if released {
		return
	}
	t.Fatal("the dropped payload survived every GC while the backing array was never " +
		"reallocated — the prune is unlinking the version without releasing it, so the " +
		"memory is reclaimed only by accident, whenever the slice next happens to grow")
}

// NON-VACUITY FOR THE FINALIZER MACHINERY ITSELF. If SetFinalizer or the GC did
// not behave as these tests assume, both would pass for the wrong reason. A
// payload nothing retains must be collected; one the test still holds must not.
func TestTheFinalizerHarnessDistinguishesReachable(t *testing.T) {
	freed := make(chan struct{})
	func() {
		p := &payload{id: -1}
		runtime.SetFinalizer(p, func(*payload) { close(freed) })
	}()
	// FINALIZERS RUN ASYNCHRONOUSLY, on their own goroutine, and not before the
	// cycle AFTER the one that finds the object unreachable. A single GC is not
	// enough — this arm caught exactly that in its own first run, which is the
	// reason it is here rather than assumed.
	if !gcUntil(freed) {
		t.Fatal("an unreferenced payload was not collected — the harness cannot detect a leak")
	}

	held := &payload{id: -2}
	stillHeld := make(chan struct{})
	runtime.SetFinalizer(held, func(*payload) { close(stillHeld) })
	runtime.GC()
	select {
	case <-stillHeld:
		t.Fatal("a payload the test still references was collected — the harness would " +
			"report success whatever Put did")
	case <-time.After(150 * time.Millisecond):
	}
	runtime.KeepAlive(held)
}
