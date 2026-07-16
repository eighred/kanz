// Package sharedstate_test is the PARITY-05c multi-replica shared-state
// contract: the capstone proof that an N-replica consumer group, once switched
// to the PARITY-04h Redis impl, produces EXACTLY-ONCE side effects under
// round-robin redelivery chaos — the property per-instance state cannot give.
//
// The seam under test is bus.RedisClient: N bus consumers share one
// bus.RedisDedup over ONE fake Redis, so a redelivery landing on a different
// replica than the original is still recognized and its side effect skipped.
//
// This tree used to exercise a SECOND seam beside dedup — integrity.RedisEval,
// the NATS<->Kafka reconciler's Lua-atomic PendingStore — because one
// redisadapter.Client satisfied both. DATA-M1 made Kafka DERIVED from NATS by a
// single archiver, so there are no longer two independent transports to
// reconcile, and DATA-M4 deleted internal/integrity outright. Dedup is the only
// shared state that survives, and it is the one that was always load-bearing.
//
// Sits in an external _test package so it consumes only the public bus surface,
// like the other contract trees. The unit tests in each package prove the seam
// in isolation; this proves the CUTOVER composition.
package sharedstate_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/pkg/bus"
)

// fakeRedis is one in-memory server standing in for the shared Redis: it
// satisfies bus.RedisClient (dedup key set), the role redisadapter.Client plays
// in production. Concurrent-safe so N replicas can hammer it.
type fakeRedis struct {
	mu   sync.Mutex
	keys map[string]time.Time // dedup: key -> expiry
	now  func() time.Time
}

func newFakeRedis(now func() time.Time) *fakeRedis {
	return &fakeRedis{keys: map[string]time.Time{}, now: now}
}

// ClaimNX models real Redis's `SET key v NX EX ttl`: the check and the set happen
// under ONE lock hold, so concurrent claimants are serialized and exactly one
// wins. Splitting them would model a Redis that does not exist.
func (f *fakeRedis) ClaimNX(_ context.Context, key string, lease time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if exp, ok := f.keys[key]; ok && !f.now().After(exp) {
		return false, nil // held
	}
	f.keys[key] = f.now().Add(lease)
	return true, nil
}

func (f *fakeRedis) SetWithTTL(_ context.Context, key string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[key] = f.now().Add(ttl)
	return nil
}

func (f *fakeRedis) Del(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.keys, key)
	return nil
}

var _ bus.RedisClient = (*fakeRedis)(nil)

// sideEffectGate models what a bus.Consumer does around a handler: atomically
// CLAIM the key, run the side effect only if the claim was won, then COMMIT. This
// is the exactly-once gate RedisDedup provides across replicas, without pulling in
// the framing internals the bus_test harness uses.
//
// It used to be Seen()-then-Record(), mirroring the old Deduper contract — and
// that check-then-act is exactly what let concurrent deliveries of one key both
// run the effect. Claim is a single atomic round-trip, so they cannot.
func sideEffectGate(d bus.Deduper, key string, effect func()) {
	if !d.Claim(key) {
		return
	}
	effect()
	d.Commit(key)
}

// TestCutover_ExactlyOnceSideEffectsUnderRedelivery is the sequential model: an
// N-replica group sharing one Redis dedup store handles repeated delivery of
// every key with exactly-once side effects.
func TestCutover_ExactlyOnceSideEffectsUnderRedelivery(t *testing.T) {
	const (
		keys         = 200
		redeliveries = 3 // each event delivered 1+3 times
	)
	now := time.Unix(1_000_000, 0)
	redis := newFakeRedis(func() time.Time { return now })

	// One shared dedup store, standing in for the N replicas' single Redis.
	dedup := bus.NewRedisDedup(redis, time.Minute)

	var mu sync.Mutex
	effects := map[string]int{}

	for k := 0; k < keys; k++ {
		key := fmt.Sprintf("evt-%04d", k)

		// Deliver the FACT 1+redeliveries times, each landing on the next replica
		// (they share the store, so the replica is immaterial to the gate — that
		// sharing is the whole point). The side effect must fire once.
		for i := 0; i < 1+redeliveries; i++ {
			sideEffectGate(dedup, key, func() {
				mu.Lock()
				effects[key]++
				mu.Unlock()
			})
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(effects) != keys {
		t.Fatalf("distinct keys with side effects = %d, want %d", len(effects), keys)
	}
	for key, n := range effects {
		if n != 1 {
			t.Errorf("key %s produced %d side effects, want exactly 1 — cross-replica redelivery not deduped", key, n)
		}
	}
}

// TestCutover_ConcurrentRedeliveryConvergesOnLTEOnce hammers the shared dedup
// with CONCURRENT redeliveries across replicas (the nak-storm case, not the
// sequential model above). The Seen/Record two-phase is non-atomic, so a
// concurrent redelivery can slip through before the first Record lands — the
// contract is "≤ a small bounded number", with idempotent handlers as the true
// floor. The property we assert: side effects never EXCEED deliveries and, for
// the overwhelming majority, collapse to exactly one.
func TestCutover_ConcurrentRedeliveryConvergesOnLTEOnce(t *testing.T) {
	const (
		keys         = 100
		redeliveries = 5
	)
	now := time.Unix(2_000_000, 0)
	redis := newFakeRedis(func() time.Time { return now })
	dedup := bus.NewRedisDedup(redis, time.Minute)

	var mu sync.Mutex
	effects := map[string]int{}

	var wg sync.WaitGroup
	for k := 0; k < keys; k++ {
		key := fmt.Sprintf("evt-%04d", k)
		for i := 0; i < 1+redeliveries; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sideEffectGate(dedup, key, func() {
					mu.Lock()
					effects[key]++
					mu.Unlock()
				})
			}()
		}
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(effects) != keys {
		t.Fatalf("distinct keys = %d, want %d", len(effects), keys)
	}
	// EXACTLY once. Not "mostly once".
	//
	// This assertion used to tolerate up to keys/4 duplicate leakage, on the grounds
	// that dedup was best-effort and idempotent handlers were the real guarantee.
	// Both halves of that were false: the leak was a check-then-act TOCTOU in the
	// deduper (Seen-then-Record), and the handler it was leaning on — the OMS order
	// path — was itself a check-then-act that routed to a VENUE, so a "tolerated"
	// duplicate was a double trade. A threshold that tolerates a race also hides it:
	// under -race the leakage ran 33-60% and the 25% bound simply flapped.
	//
	// Claim is atomic, so the correct bound is zero, and a bound of zero is the only
	// one that cannot silently drift.
	for key, n := range effects {
		if n != 1 {
			t.Errorf("key %s produced %d side effects, want exactly 1 — every extra one is a duplicate dispatch", key, n)
		}
	}
}
