package bus

import (
	"sync"
	"time"
)

// defaultClaimLease bounds how long a claimed-but-unfinished key stays claimed.
//
// It is the crash window, and it is the whole reason this is a LEASE and not a
// plain set-if-absent. A worker that claims a key and then dies would, with an
// unbounded claim, leave that event permanently deduped — the redelivery would be
// skipped and the event silently lost. The lease expires instead, so the event is
// reprocessed. Short enough that a crash costs seconds, long enough to cover a
// normal dispatch including its retries.
//
// A dispatch that outlives its lease can be claimed concurrently by a second
// delivery — the lease bounds the exposure, it does not eliminate it. Size it above
// RetryConfig's worst-case total backoff for handlers that need longer.
const defaultClaimLease = 5 * time.Second

// Deduper is the consumer's exactly-once-ish guard around a dispatch. It is a
// three-phase LEASE, not a check-then-act:
//
//	Claim   — atomically take the key. false ⇒ someone else has it or finished it.
//	Commit  — the dispatch succeeded; hold the key for the full dedup window.
//	Release — the dispatch failed; drop the key so a redelivery can retry it.
//
// The shape matters. The previous contract was Seen()+Record(), with Record called
// only after a successful dispatch so that a failure could not poison the window
// against retries. That preserved retries but left a TOCTOU window: two concurrent
// deliveries of one key both passed Seen before either Recorded, and both ran the
// handler. On this platform a handler is `route the order to the venue`, so that
// window is a double trade, and it was measurable — the shared-state contract test
// saw 33–60% of keys leak duplicates under load.
//
// Claim closes the window without reintroducing the loss: the claim is atomic (one
// Redis round-trip, SET NX EX), and a failed dispatch RELEASES rather than
// committing, so the retry semantics the old split existed to protect are intact.
//
// A nil Deduper is a no-op: Claim returns true (proceed), Commit and Release do
// nothing. So "dedup disabled" needs no nil-checks at the call site.
//
// Implementations: the in-memory per-instance DedupWindow (default) and the
// distributed RedisDedup (DEBT-02a, opt-in via WithDeduper) for cross-pod dedup.
type Deduper interface {
	// Claim atomically takes key for processing. It reports true when the caller
	// now owns the key, and false when the key is already claimed (a concurrent
	// delivery) or already committed (a prior delivery completed it) — in which
	// case the caller must skip the dispatch.
	Claim(key string) bool
	// Commit marks key completed, holding it for the full dedup window.
	Commit(key string)
	// Release relinquishes a claim after a failed dispatch, so a redelivery is
	// free to retry it.
	Release(key string)
}

// Compile-time assertion the in-memory window satisfies the interface.
var _ Deduper = (*DedupWindow)(nil)

// DedupWindow is a bounded window of idempotency_keys claimed by a consumer
// instance. It is in-memory and per-instance — cross-pod dedup is NOT in scope
// here (use RedisDedup for that, DEBT-02a). Within one instance its Claim is
// exact: the map is taken under a mutex, so two concurrent deliveries of the same
// key cannot both claim it.
//
// Sized by TTL (correctness window — default matches NATS JetStream's broker-side
// dedup so the layers reinforce) and max entry count (memory protection). On
// overflow, expired entries are GC'd; if still over capacity, the entry expiring
// soonest is evicted.
type DedupWindow struct {
	mu    sync.Mutex
	ttl   time.Duration
	lease time.Duration
	max   int
	// expiry maps a key to the instant it stops suppressing dispatches — a lease
	// deadline while in flight, the full TTL once committed.
	expiry map[string]time.Time
}

// NewDedupWindow returns a configured window, or nil when ttl<=0 or max<=0
// (i.e. dedup disabled). The methods on *DedupWindow handle a nil receiver
// as a no-op, so callers don't need to nil-check at every use site.
func NewDedupWindow(ttl time.Duration, max int) *DedupWindow {
	if ttl <= 0 || max <= 0 {
		return nil
	}
	lease := defaultClaimLease
	if ttl < lease {
		lease = ttl // a lease longer than the window it lives in makes no sense
	}
	return &DedupWindow{ttl: ttl, lease: lease, max: max, expiry: make(map[string]time.Time)}
}

// Claim atomically takes key, leasing it for the claim lease.
func (w *DedupWindow) Claim(key string) bool {
	if w == nil || key == "" {
		return true // dedup disabled ⇒ every delivery proceeds
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if exp, ok := w.expiry[key]; ok && now.Before(exp) {
		return false // in flight elsewhere, or already committed
	}
	if len(w.expiry) >= w.max {
		w.gc(now)
	}
	w.expiry[key] = now.Add(w.lease)
	return true
}

// Commit holds key for the full dedup window.
func (w *DedupWindow) Commit(key string) {
	if w == nil || key == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.expiry[key] = time.Now().Add(w.ttl)
}

// Release drops key so a redelivery can retry it.
func (w *DedupWindow) Release(key string) {
	if w == nil || key == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.expiry, key)
}

// gc sweeps expired entries; if still at capacity, evicts the one expiring
// soonest. Caller holds w.mu.
func (w *DedupWindow) gc(now time.Time) {
	for k, exp := range w.expiry {
		if !now.Before(exp) {
			delete(w.expiry, k)
		}
	}
	for len(w.expiry) >= w.max {
		var soonestK string
		var soonestT time.Time
		for k, exp := range w.expiry {
			if soonestK == "" || exp.Before(soonestT) {
				soonestK, soonestT = k, exp
			}
		}
		delete(w.expiry, soonestK)
	}
}
