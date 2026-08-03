package bus

import "time"

// This file is compiled only for this package's tests. It exposes the few
// internals the black-box tests in package bus_test need in order to keep
// asserting behaviour that #237 pushed out of wall-clock reach.

// SetDedupWindowClock replaces a window's time source so lease and TTL expiry
// can be driven deterministically. Necessary since the claim lease became
// dedupClaimLease (maxTunedAckWait + margin, 75s): the previous tests built a
// 50ms window and slept, which relied on NewDedupWindow clamping the LEASE down
// to a short TTL — the exact inversion this change removed.
func SetDedupWindowClock(w *DedupWindow, now func() time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = now
}

// DedupClaimLease is the in-process claim lease, for tests asserting its
// relationship to the tuned AckWaits.
const DedupClaimLease = dedupClaimLease

// MaxTunedAckWait is the longest AckWait any built-in consumer class uses.
const MaxTunedAckWait = maxTunedAckWait

// MinTunedAckWait is the shortest AckWait any built-in consumer class uses.
const MinTunedAckWait = minTunedAckWait

// RedisClaimLease is the cross-pod claim lease.
const RedisClaimLease = redisClaimLease
