// Package sharedstate_test is the PARITY-05c multi-replica shared-state
// contract: the capstone proof that an N-replica consumer group, once switched
// to the PARITY-04h Redis impls, produces EXACTLY-ONCE side effects and
// reconciles cross-transport under round-robin redelivery chaos — the property
// per-instance state cannot give.
//
// The two shared-state seams are exercised TOGETHER over ONE fake Redis (as in
// production, where a single redisadapter.Client satisfies both bus.RedisClient
// and integrity.RedisEval):
//
//   - dedup: N bus consumers share one bus.RedisDedup, so a redelivery landing
//     on a different replica than the original is still recognized and its side
//     effect skipped;
//   - reconcile: N reconcilers share one integrity.RedisPendingStore, so a NATS
//     sighting on one pod matches the Kafka sighting on another.
//
// Sits in an external _test package so it consumes only the public bus +
// integrity surface, like the other contract trees. The unit tests in each
// package prove the seams in isolation; this proves the CUTOVER composition.
package sharedstate_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/internal/integrity"
	"github.com/kanz-eng/kanz/pkg/bus"
)

// fakeRedis is one in-memory server standing in for the shared Redis: it
// satisfies bus.RedisClient (dedup key set) AND integrity.RedisEval (the
// reconciler pending-hash Lua scripts), the exact dual role redisadapter.Client
// plays in production. Concurrent-safe so N replicas can hammer it.
type fakeRedis struct {
	mu      sync.Mutex
	keys    map[string]time.Time // dedup: key -> expiry
	pending map[string]string    // reconcile hash: field -> "<transportInt>:<firstSeenNano>"
	now     func() time.Time
}

func newFakeRedis(now func() time.Time) *fakeRedis {
	return &fakeRedis{keys: map[string]time.Time{}, pending: map[string]string{}, now: now}
}

// --- bus.RedisClient (dedup) ---

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

// --- integrity.RedisEval (reconcile) — the three PendingStore scripts by arity ---

func (f *fakeRedis) Eval(_ context.Context, _ string, _ []string, args ...any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch len(args) {
	case 3: // claim: key, transportInt, nowNano
		field, transport, nano := args[0].(string), args[1].(string), args[2].(string)
		v, ok := f.pending[field]
		if !ok {
			f.pending[field] = transport + ":" + nano
			return "P", nil
		}
		if strings.SplitN(v, ":", 2)[0] == transport {
			return "D", nil
		}
		delete(f.pending, field)
		return "M:" + v, nil
	case 1: // sweep: cutoffNano
		cutoff, _ := strconv.ParseInt(args[0].(string), 10, 64)
		var out []any
		for field, v := range f.pending {
			seen, _ := strconv.ParseInt(strings.SplitN(v, ":", 2)[1], 10, 64)
			if seen < cutoff {
				out = append(out, v+":"+field)
				delete(f.pending, field)
			}
		}
		return out, nil
	default: // count
		return int64(len(f.pending)), nil
	}
}

var (
	_ bus.RedisClient     = (*fakeRedis)(nil)
	_ integrity.RedisEval = (*fakeRedis)(nil)
)

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

// TestCutover_ExactlyOnceSideEffectsAndReconcileUnderChaos is the flagship
// proof: an N-replica group sharing the Redis dedup + reconcile stores handles
// round-robin redelivery on BOTH transports with exactly-once side effects and
// full cross-instance reconciliation.
func TestCutover_ExactlyOnceSideEffectsAndReconcileUnderChaos(t *testing.T) {
	const (
		replicas     = 4
		keys         = 200
		redeliveries = 3 // each event delivered 1+3 times
	)
	now := time.Unix(1_000_000, 0)
	redis := newFakeRedis(func() time.Time { return now })

	// One shared dedup store + one shared pending store, N reconciler replicas.
	dedup := bus.NewRedisDedup(redis, time.Minute)
	pending := integrity.NewRedisPendingStore(redis)
	recons := make([]*integrity.Reconciler, replicas)
	for i := range recons {
		recons[i] = integrity.NewReconcilerWithStore(pending, time.Minute, func() time.Time { return now })
	}

	var mu sync.Mutex
	effects := map[string]int{}
	matched := map[string]int{}

	rr := 0 // round-robin replica cursor
	for k := 0; k < keys; k++ {
		key := fmt.Sprintf("evt-%04d", k)
		env := &envelopepb.Envelope{IdempotencyKey: key}

		// Deliver the FACT 1+redeliveries times, each landing on the next replica
		// (round-robin), on the NATS hot path. The side effect must fire once.
		for i := 0; i < 1+redeliveries; i++ {
			sideEffectGate(dedup, key, func() {
				mu.Lock()
				effects[key]++
				mu.Unlock()
			})
			// Every NATS delivery is also observed by the reconciler on that pod.
			recons[rr%replicas].Observe(integrity.TransportNATS, env)
			rr++
		}

		// The SAME event arrives on the Kafka log of record, on a DIFFERENT pod
		// than saw it first — it must match cross-instance through the shared store.
		res := recons[rr%replicas].Observe(integrity.TransportKafka, env)
		rr++
		if res.Status == integrity.ReconcileMatched {
			mu.Lock()
			matched[key]++
			mu.Unlock()
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
	for key := 0; key < keys; key++ {
		if matched[fmt.Sprintf("evt-%04d", key)] != 1 {
			t.Errorf("evt-%04d was not reconciled cross-instance exactly once", key)
		}
	}
	// Every event matched on both transports ⇒ the shared pending set is drained.
	if pending.Count() != 0 {
		t.Errorf("pending set = %d after full reconciliation, want 0", pending.Count())
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
		replicas     = 8
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
