package decisionq_test

import (
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/decisionq"
)

// THE THREE PROPERTIES BOTH CALLERS DEPEND ON (#713).
//
// This queue sits in front of the audit trail for two hot paths — every
// authorization check and every order admission — so its failures are all of the
// quiet kind: a blocked Enqueue stalls the service it was protecting, a discarded
// buffer loses the decisions made just before a restart, and an uncounted drop
// leaves a trail with holes nobody can see.

// ENQUEUE NEVER BLOCKS, even when nothing is draining. This is the property the
// whole package exists for: the caller is on a request or an order path and must
// not wait for a broker.
func TestEnqueueNeverBlocksWhenTheWorkerIsWedged(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	q := decisionq.New(func(int) { <-release }, decisionq.WithSize[int](1))
	defer func() {
		once.Do(func() { close(release) })
		q.Close()
	}()

	done := make(chan struct{})
	go func() {
		// One is taken by the wedged worker, one fills the buffer, the rest must
		// all be dropped rather than block.
		for i := 0; i < 100; i++ {
			q.Enqueue(i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue blocked with the worker wedged and the buffer full — the caller's hot path " +
			"is now waiting on the audit sink, which is the outage this queue exists to prevent")
	}
	if q.Dropped() == 0 {
		t.Error("nothing was counted as dropped, so the records went somewhere unobserved")
	}
}

// EVERY DROP IS COUNTED AND REPORTED. A drop nobody counts is an audit trail with
// holes that look like quiet periods.
func TestOverflowIsCountedAndReported(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var dropped []int
	q := decisionq.New(func(int) { <-release },
		decisionq.WithSize[int](1),
		decisionq.WithOverflowHandler(func(v int) {
			mu.Lock()
			defer mu.Unlock()
			dropped = append(dropped, v)
		}))
	for i := 0; i < 20; i++ {
		q.Enqueue(i)
	}
	close(release)
	q.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(dropped) == 0 {
		t.Fatal("the overflow handler never fired, so a full buffer sheds records silently")
	}
	if int64(len(dropped)) != q.Dropped() {
		t.Errorf("the handler saw %d drops and the counter says %d — an operator reading the metric "+
			"and an operator reading the log would disagree about how much of the trail is missing",
			len(dropped), q.Dropped())
	}
}

// CLOSE DRAINS. The decisions still buffered were made and acted upon; dropping
// them on the way out would leave the trail short exactly on the restarts
// somebody will later want to reconstruct.
func TestCloseDrainsWhatIsAlreadyQueued(t *testing.T) {
	var mu sync.Mutex
	var got []int
	q := decisionq.New(func(v int) {
		time.Sleep(time.Millisecond) // a slow sink: without a drain these are lost
		mu.Lock()
		defer mu.Unlock()
		got = append(got, v)
	})
	for i := 0; i < 25; i++ {
		q.Enqueue(i)
	}
	q.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 25 {
		t.Fatalf("published %d of 25 queued records before shutdown completed — the rest were "+
			"discarded by Close", len(got))
	}
	// AND IN ORDER. One worker rather than a pool is a correctness choice: both
	// callers partition their events so one subject's trail stays ordered.
	for i, v := range got {
		if v != i {
			t.Fatalf("record %d published at position %d — the queue reordered a trail a reader "+
				"reconstructs a sequence from", v, i)
		}
	}
}

// CLOSE IS IDEMPOTENT, because a composition root that defers it and also calls
// it on a shutdown path must not panic on the second one.
func TestCloseTwiceIsSafe(t *testing.T) {
	q := decisionq.New(func(int) {})
	q.Close()
	q.Close()
}

// A NON-POSITIVE SIZE IS IGNORED, NOT TAKEN LITERALLY. An unbuffered channel
// would make every Enqueue drop unless a worker happened to be waiting, turning a
// tuning mistake into the silent loss of the whole trail.
func TestANonPositiveSizeFallsBackToTheDefault(t *testing.T) {
	release := make(chan struct{})
	q := decisionq.New(func(int) { <-release }, decisionq.WithSize[int](0))
	defer func() { close(release); q.Close() }()

	// One is taken by the worker; the rest must fit the default buffer.
	for i := 0; i < 100; i++ {
		q.Enqueue(i)
	}
	if q.Dropped() != 0 {
		t.Errorf("%d records dropped with a size of 0 — the queue was built unbuffered and sheds "+
			"almost everything", q.Dropped())
	}
}
