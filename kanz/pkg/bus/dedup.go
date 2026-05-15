package bus

import (
	"sync"
	"time"
)

// DedupWindow is a bounded sliding window of idempotency_keys seen by a
// consumer instance. It is in-memory and per-instance — cross-pod dedup is
// NOT in scope; the canonical defense against cross-instance redelivery is
// idempotent handlers (event-class-rules §1). This window is belt-and-braces
// against in-instance redelivery and a perf optimization that lets a
// successful handler skip redundant work.
//
// Sized by TTL (correctness window — default matches NATS JetStream's
// broker-side dedup so the layers reinforce) and max entry count (memory
// protection). On overflow, expired entries are GC'd; if still over
// capacity, the oldest entry is evicted.
//
// Concurrency note: Seen and Record are two separate calls so the Consumer
// can record only after a *successful* dispatch — a handler failure must not
// poison the window against retries. Two concurrent deliveries of the same
// key can both pass Seen before either Records; handlers must be idempotent
// (per event-class-rules §1) to cope.
type DedupWindow struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	seen map[string]time.Time
}

// NewDedupWindow returns a configured window, or nil when ttl<=0 or max<=0
// (i.e. dedup disabled). The methods on *DedupWindow handle a nil receiver
// as a no-op, so callers don't need to nil-check at every use site.
func NewDedupWindow(ttl time.Duration, max int) *DedupWindow {
	if ttl <= 0 || max <= 0 {
		return nil
	}
	return &DedupWindow{ttl: ttl, max: max, seen: make(map[string]time.Time)}
}

// Seen reports whether key was Recorded within the last ttl. Read-only.
func (w *DedupWindow) Seen(key string) bool {
	if w == nil || key == "" {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.seen[key]
	return ok && time.Since(t) < w.ttl
}

// Record marks key as seen at the current time. Empty keys are ignored.
func (w *DedupWindow) Record(key string) {
	if w == nil || key == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if len(w.seen) >= w.max {
		w.gc(now)
	}
	w.seen[key] = now
}

// gc sweeps expired entries; if still at capacity, evicts the oldest one.
// Caller holds w.mu.
func (w *DedupWindow) gc(now time.Time) {
	for k, t := range w.seen {
		if now.Sub(t) >= w.ttl {
			delete(w.seen, k)
		}
	}
	for len(w.seen) >= w.max {
		var oldestK string
		var oldestT time.Time
		for k, t := range w.seen {
			if oldestK == "" || t.Before(oldestT) {
				oldestK, oldestT = k, t
			}
		}
		delete(w.seen, oldestK)
	}
}
