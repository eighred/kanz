package state

import (
	"sync"
	"time"
)

// dedupWindow is a bounded sliding window of seen idempotency_keys
// scoped to ONE portfolio. The bus.Consumer's global DedupWindow
// (EVT-17d) catches most replays at the broker boundary, but a
// per-aggregate window is the defense-in-depth layer that catches:
//
//   - Multiple bus.Consumers feeding the same Applier (one per
//     subject — the typical orchestrator wiring).
//   - Process restarts that drop the bus dedup window but leave
//     committed portfolio state intact.
//
// Each Portfolio gets its own dedupWindow. Memory cost is O(maxSize)
// per portfolio; per-portfolio scoping matches event-class-rules §1
// ("dedup against a bounded window sized to the broker's redelivery
// window") without conflating keys across aggregates.
type dedupWindow struct {
	mu      sync.Mutex
	seen    map[string]time.Time
	order   []string
	maxSize int
	ttl     time.Duration
	now     func() time.Time // injected for tests
}

const (
	// Defaults match bus.DedupWindow (EVT-17d) so the two layers
	// reinforce — a duplicate within 2m is caught by either.
	defaultDedupTTL = 2 * time.Minute
	defaultDedupMax = 10_000
)

func newDedupWindow() *dedupWindow {
	return &dedupWindow{
		seen:    make(map[string]time.Time),
		maxSize: defaultDedupMax,
		ttl:     defaultDedupTTL,
		now:     time.Now,
	}
}

// Seen reports whether the key is in the window, GC'ing expired
// entries opportunistically before the check.
func (w *dedupWindow) Seen(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gc()
	_, ok := w.seen[key]
	return ok
}

// Record inserts the key and evicts the oldest if the window is at
// capacity. Caller must invoke Seen before Record so the two-step
// "check then commit" matches the bus.Consumer pattern (a transient
// apply failure must remain retryable, which means recording happens
// only after successful apply).
func (w *dedupWindow) Record(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gc()
	if _, ok := w.seen[key]; ok {
		return
	}
	w.seen[key] = w.now()
	w.order = append(w.order, key)
	if len(w.order) > w.maxSize {
		oldest := w.order[0]
		delete(w.seen, oldest)
		w.order = w.order[1:]
	}
}

// Keys returns a copy of the live keys, oldest-first, after GC. This is
// the durable dedup tail state.Store.SnapshotWithKeys persists so the
// bootstrap path (PERS-01d) can re-seed the window and skip replayed
// boundary events. Called under the per-portfolio lock so it observes the
// same window the concurrent apply path mutates.
func (w *dedupWindow) Keys() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gc()
	out := make([]string, len(w.order))
	copy(out, w.order)
	return out
}

// gc drops expired entries from the front of order. Lazy — runs at
// each Seen / Record so the worst-case behavior tracks call volume
// rather than wall-clock time.
func (w *dedupWindow) gc() {
	cutoff := w.now().Add(-w.ttl)
	i := 0
	for i < len(w.order) {
		ts, ok := w.seen[w.order[i]]
		if !ok || ts.Before(cutoff) {
			delete(w.seen, w.order[i])
			i++
			continue
		}
		break
	}
	w.order = w.order[i:]
}
