package bus_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
)

// DEBT-02d — an N-replica consumer group under redelivery chaos produces no
// duplicate side-effects when the replicas share a distributed deduper
// (RedisDedup, DEBT-02a): a redelivery landing on a DIFFERENT replica than the
// original is still recognized as a duplicate and skipped — the property
// per-instance DedupWindow cannot give (the contrast test below shows it leaks).
//
// capturingSub hands the consumer's fully-wrapped inner handler back to the test
// so deliveries can be driven explicitly across replicas; each call runs the
// real unframe→validate→dedup→handler→record pipeline.
type capturingSub struct{ out chan bus.Handler }

func (s *capturingSub) Subscribe(ctx context.Context, _, _ string, h bus.Handler) error {
	s.out <- h
	<-ctx.Done() // block like a real subscriber until shutdown
	return nil
}

// replicaGroup spins up n consumers (each built with mkConsumer, so a test
// chooses shared vs per-instance dedup) sharing one side-effect recorder, and
// returns their captured inner handlers + the per-key effect counts.
func replicaGroup(t *testing.T, ctx context.Context, n int, mkConsumer func(bus.Subscriber) *bus.Consumer) ([]bus.Handler, map[string]int, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	effects := make(map[string]int)
	record := func(_ context.Context, env *envelopepb.Envelope, _ []byte) error {
		mu.Lock()
		effects[env.IdempotencyKey]++
		mu.Unlock()
		return nil
	}
	handlers := make([]bus.Handler, 0, n)
	for i := 0; i < n; i++ {
		sub := &capturingSub{out: make(chan bus.Handler, 1)}
		c := mkConsumer(sub)
		go func() { _ = c.Subscribe(ctx, "risk.portfolio.snapshot", "risk-engine", record) }()
		handlers = append(handlers, <-sub.out)
	}
	return handlers, effects, &mu
}

// frameKey builds a valid framed FACT message whose idempotency_key is key.
func frameKey(t *testing.T, key string) bus.Message {
	t.Helper()
	env := validEnvelope()
	env.EventId = key
	env.CorrelationId = key
	env.IdempotencyKey = key // FACT: idempotency_key == event_id
	return bus.Message{Subject: "risk.portfolio.snapshot", Body: frame(t, env, []byte("p"))}
}

// deliverChaos delivers each of `keys` keys 1+redeliveries times, round-robin
// across replicas so originals and redeliveries hit different pods. Sequential
// per delivery (the realistic nak→later-redelivery model: a redelivery's Seen
// check runs after the prior dispatch's Record).
func deliverChaos(t *testing.T, ctx context.Context, handlers []bus.Handler, keys, redeliveries int) {
	t.Helper()
	d := 0
	for k := 0; k < keys; k++ {
		m := frameKey(t, fmt.Sprintf("evt-%03d", k))
		for i := 0; i < 1+redeliveries; i++ {
			if err := handlers[d%len(handlers)](ctx, m); err != nil {
				t.Fatalf("delivery failed: %v", err)
			}
			d++
		}
	}
}

func TestNReplicaSharedDedupNoDuplicateSideEffects(t *testing.T) {
	const (
		replicas, keys, redeliveries = 4, 50, 3
	)

	shared := bus.NewRedisDedup(newFakeRedis(), time.Minute) // one store, all pods
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handlers, effects, mu := replicaGroup(t, ctx, replicas, func(s bus.Subscriber) *bus.Consumer {
		c, err := bus.NewConsumer(s, bus.WithDeduper(shared))
		if err != nil {
			t.Fatalf("NewConsumer: %v", err)
		}
		return c
	})

	deliverChaos(t, ctx, handlers, keys, redeliveries)

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

// Contrast: per-instance DedupWindow (the default, no shared store) does NOT
// dedup a redelivery that lands on another replica — proving the distributed
// deduper is what closes the gap. Documents the failure mode, not an aspiration.
func TestNReplicaPerInstanceDedupLeaksDuplicates(t *testing.T) {
	const (
		replicas, keys, redeliveries = 4, 50, 3
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handlers, effects, mu := replicaGroup(t, ctx, replicas, func(s bus.Subscriber) *bus.Consumer {
		c, err := bus.NewConsumer(s) // default per-instance DedupWindow
		if err != nil {
			t.Fatalf("NewConsumer: %v", err)
		}
		return c
	})

	deliverChaos(t, ctx, handlers, keys, redeliveries)

	mu.Lock()
	defer mu.Unlock()
	total := 0
	for _, n := range effects {
		total += n
	}
	// Round-robin spreads each key's deliveries across distinct replicas, so each
	// replica's own window never fires — duplicates leak. The assertion is just
	// "more side effects than unique keys", robust to the exact count.
	if total <= keys {
		t.Fatalf("expected duplicate side effects with per-instance dedup, got total=%d for %d keys", total, keys)
	}
}
