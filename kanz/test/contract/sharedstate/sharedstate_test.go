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

func (f *fakeRedis) Exists(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	exp, ok := f.keys[key]
	if !ok {
		return false, nil
	}
	if f.now().After(exp) {
		delete(f.keys, key)
		return false, nil
	}
	return true, nil
}

func (f *fakeRedis) SetWithTTL(_ context.Context, key string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[key] = f.now().Add(ttl)
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

// sideEffectGate models what a bus.Consumer does around a handler: check the
// (shared) deduper, and only run the side effect + Record on first sight. This
// is the exactly-once gate the RedisDedup provides across replicas, without
// pulling in the framing internals the bus_test harness uses.
func sideEffectGate(d bus.Deduper, key string, effect func()) {
	if d.Seen(key) {
		return
	}
	effect()
	d.Record(key)
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
	over := 0
	for _, n := range effects {
		if n < 1 {
			t.Fatalf("a key produced %d side effects (<1) — dedup dropped a real event", n)
		}
		if n > 1+redeliveries {
			t.Fatalf("a key produced %d side effects, exceeding deliveries", n)
		}
		if n > 1 {
			over++
		}
	}
	// The shared store collapses the vast majority even under concurrency; a few
	// races are tolerated (idempotent handlers are the real guarantee). Guard a
	// gross regression, not the exact race count.
	if over > keys/4 {
		t.Errorf("%d/%d keys leaked duplicates under concurrency — dedup not effective", over, keys)
	}
}
