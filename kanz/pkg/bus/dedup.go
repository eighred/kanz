package bus

import (
	"sync"
	"time"
)

// claimLeaseMargin is the headroom the in-process claim lease keeps ABOVE the
// longest AckWait any consumer in this estate is configured with. It exists so
// the lease is still holding when the broker's redelivery arrives — not so it is
// "about" to expire.
const claimLeaseMargin = 15 * time.Second

// dedupClaimLease bounds how long a claimed-but-unfinished key stays claimed in
// the IN-PROCESS window (DedupWindow). It is DERIVED from maxTunedAckWait rather
// than chosen independently, and that derivation is the whole point.
//
// WHAT WENT WRONG WHEN IT WAS AN INDEPENDENT NUMBER (#237). It was a flat 5s
// while JetStream's AckWait was the 30s server default. Claim's own doc says why
// the claim exists: on this platform a handler is `route the order to the venue`,
// so two concurrent dispatches of one event are a double trade, and it was
// measurable — 33–60% of keys leaked duplicates under load. But an in-flight
// dispatch holds only the LEASE; the full TTL is held only after Commit. So at
// the 30s redelivery the lease had been expired for 25s, Claim returned true, and
// the redelivery ran the handler a second time CONCURRENTLY WITH THE FIRST. The
// guard was not weak, it was inverted: it stood down exactly when it was needed.
// services/oms/internal/order/orderlock.go sized its own wait on the opposite
// premise, in a comment that was simply false.
//
// WHY THE RELATIONSHIP IS NOW STRUCTURAL AND NOT A COMMENT. Three things, in
// descending order of how much they can be ignored:
//
//  1. The lease is maxTunedAckWait + margin, so raising any class's AckWait in
//     tuning.go raises the lease with it. There is no second number to remember.
//  2. The compile-time assertion below fails the BUILD, not a test, if the
//     ordering is ever inverted by editing either constant.
//  3. ConsumerTuning.validate refuses any NATSConfig.ConsumerTuning override
//     whose AckWait reaches the lease, naming the consequence — the #242 pattern
//     from pkg/auth/oidc.go, where a two-interval ordering is refused at
//     construction rather than left to thrash in production.
//
// THE CRASH WINDOW, WHICH IS WHY THIS IS A LEASE AND NOT A PLAIN SET-IF-ABSENT.
// A worker that claims a key and then dies never Commits and never Releases; an
// unbounded claim would leave that event permanently deduped and the redelivery
// silently discarded. For THIS window that cannot happen — it is per-process, so
// a crash takes the map with it and every claim vanishes at once. That is
// precisely why a lease longer than AckWait is free here and NOT free for the
// cross-pod RedisDedup, whose claims outlive the pod that took them; see
// redisClaimLease for the opposite conclusion and its reasoning.
const dedupClaimLease = maxTunedAckWait + claimLeaseMargin

// Compile-time assertion: the in-process claim lease must OUTLAST the longest
// AckWait, or a redelivery arrives to find the key free and dispatches a second
// concurrent copy of an event whose first copy is still running. Inverting the
// two constants makes this expression negative and the package stops compiling.
const _ = uint(dedupClaimLease - maxTunedAckWait - 1)

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
	// now is the time source, swappable only from this package's tests
	// (export_test.go). It exists because the lease is now derived from
	// maxTunedAckWait and is 75s: expiry and eviction used to be testable by
	// building a 50ms window and sleeping, and the whole point of #237 is that a
	// window that short can no longer exist. A seam is cheaper than deleting the
	// tests that prove a stranded claim recovers at all.
	now func() time.Time
	// expiry maps a key to the instant it stops suppressing dispatches — a lease
	// deadline while in flight, the full TTL once committed.
	expiry map[string]time.Time
}

// NewDedupWindow returns a configured window, or nil when ttl<=0 or max<=0
// (i.e. dedup disabled). The methods on *DedupWindow handle a nil receiver
// as a no-op, so callers don't need to nil-check at every use site.
//
// A ttl shorter than the claim lease RAISES THE TTL rather than shortening the
// lease. This used to clamp the other way — "a lease longer than the window it
// lives in makes no sense" — and that was the inversion in miniature: shortening
// the lease to fit a small window is exactly how the claim stops covering the
// redelivery it exists to cover. The window is a suppression horizon and may be
// widened harmlessly; the lease is a correctness bound tied to AckWait and may
// not be narrowed. See dedupClaimLease.
func NewDedupWindow(ttl time.Duration, max int) *DedupWindow {
	if ttl <= 0 || max <= 0 {
		return nil
	}
	if ttl < dedupClaimLease {
		ttl = dedupClaimLease
	}
	return &DedupWindow{ttl: ttl, lease: dedupClaimLease, max: max, expiry: make(map[string]time.Time), now: time.Now}
}

// Claim atomically takes key, leasing it for the claim lease.
func (w *DedupWindow) Claim(key string) bool {
	if w == nil || key == "" {
		return true // dedup disabled ⇒ every delivery proceeds
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.clock()
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
	w.expiry[key] = w.clock().Add(w.ttl)
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

// clock is the window's time source: w.now when a test has installed one,
// time.Now otherwise. Caller holds w.mu (or is the constructor).
func (w *DedupWindow) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}
