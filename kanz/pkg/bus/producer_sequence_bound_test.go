package bus

import (
	"context"
	"fmt"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
)

// THE SEQUENCE MAP IS BOUNDED (#805).
//
// # What it cost
//
// Producer.sequence is keyed by {event_type, partition_key}, and partition_key
// ON THE ORDER PATH IS THE ORDER ID — the OMS stamps it in the one builder every
// order FACT goes through, and the api-gateway, both venue adapters and
// optimization do the same. One Producer per process, no evictor, and the only
// delete( in this package belonged to DedupWindow. So roughly 2-6 permanent
// entries per order the process had ever touched, for the life of the pod.
//
// It is the platform's highest-throughput long-lived map and it was monotonic.
// At institutional rates the OMS heap grows until the pod is OOM-killed — which
// on the execution path means orders in flight at an unknown state and a restart
// that has to reconcile them. It arrives as a memory eviction rather than an
// error, so nothing on the trading path reports it until the pod dies.
//
// # What these assert
//
// The issue's own "Verified when": publish across N distinct partition keys,
// advance past the retention bound, and len(sequence) is BOUNDED rather than N.
// Plus the two properties that make the bound safe to have — a live key still
// counts monotonically, and an evicted key restarts at 1 rather than 0, because
// Validate refuses 0 while partition_key is set.

// countingClient accepts everything and remembers nothing but the count.
type countingClient struct{ n int }

func (c *countingClient) Publish(context.Context, Message) error { c.n++; return nil }
func (c *countingClient) Subscribe(context.Context, string, string, Handler) error {
	return fmt.Errorf("not implemented")
}
func (c *countingClient) Close() error { return nil }

// boundedProducer is a Producer with a small ceiling and a clock a test drives.
func boundedProducer(t *testing.T, ttl time.Duration, max int) (*Producer, *time.Time) {
	t.Helper()
	p, err := NewProducer(&countingClient{}, ProducerConfig{
		Source: "test", ProducerVersion: "v0", Tenant: "acme",
		SequenceTTL: ttl, SequenceMax: max,
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	at := time.Unix(1_700_000_000, 0).UTC()
	p.now = func() time.Time { return at }
	return p, &at
}

// seqEvent is a minimal valid event on one partition key.
func seqEvent(key string) Event {
	return Event{
		Subject:       "observability.decision.logged",
		EventType:     "observability.decision.logged",
		EventClass:    envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion: 1,
		Domain:        "observability",
		EventTime:     time.Unix(1_700_000_000, 0).UTC(),
		PartitionKey:  key,
		Payload:       &observationpb.DecisionLog{Decider: "test"},
	}
}

func seqLen(p *Producer) int {
	p.seqMu.Lock()
	defer p.seqMu.Unlock()
	return len(p.sequence)
}

// THE ISSUE'S "VERIFIED WHEN". N distinct keys, then past the bound: the map is
// bounded rather than N.
func TestTheSequenceMapIsBoundedRatherThanGrowingWithKeys(t *testing.T) {
	const max = 50
	p, clock := boundedProducer(t, time.Hour, max)
	ctx := context.Background()

	// Every one of these is a distinct order id, which is what the OMS stamps.
	for i := 0; i < max*4; i++ {
		if err := p.Publish(ctx, seqEvent(fmt.Sprintf("order-%d", i))); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	if got := seqLen(p); got > max {
		t.Fatalf("the sequence map holds %d keys with a ceiling of %d.\n\n"+
			"This is #805: one entry per (event type x order id), for the life of the pod, in the "+
			"platform's highest-throughput long-lived map. The OMS heap grows until the pod is "+
			"OOM-killed, which on the execution path means orders in flight at an unknown state "+
			"and a restart that has to reconcile them.", got, max)
	}

	// And time passing sheds the idle ones rather than merely capping them.
	*clock = clock.Add(2 * time.Hour)
	if err := p.Publish(ctx, seqEvent("order-after-the-ttl")); err != nil {
		t.Fatalf("publish after the TTL: %v", err)
	}
	if got := seqLen(p); got > max {
		t.Fatalf("after the TTL the map still holds %d keys, ceiling %d", got, max)
	}
}

// A LIVE KEY STILL COUNTS. Without this the test above passes on a producer that
// evicted everything on every publish, which would make producer_sequence
// constantly 1 and the field meaningless.
func TestALiveKeyKeepsCounting(t *testing.T) {
	p, _ := boundedProducer(t, time.Hour, 50)
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		if err := p.Publish(ctx, seqEvent("order-live")); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		p.seqMu.Lock()
		got := p.sequence[seqKey{"observability.decision.logged", "order-live"}].seq
		p.seqMu.Unlock()
		if got != uint64(i) {
			t.Fatalf("publish %d stamped sequence %d — a key that is still live must count "+
				"monotonically, or the field says nothing about ordering", i, got)
		}
	}
}

// AN EVICTED KEY RESTARTS AT 1, NEVER 0.
//
// Validate refuses producer_sequence == 0 while partition_key is non-empty, so
// an eviction that returned 0 would turn a memory bound into a publish failure
// on the order path. 1 is also exactly what a fresh process would stamp, which
// is the restart the envelope contract has always tolerated.
func TestAnEvictedKeyRestartsAtOneAndStillPublishes(t *testing.T) {
	p, clock := boundedProducer(t, time.Hour, 50)
	ctx := context.Background()

	if err := p.Publish(ctx, seqEvent("order-idle")); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	// Idle past the TTL, then push the map to its ceiling so a sweep runs.
	*clock = clock.Add(2 * time.Hour)
	for i := 0; i < 60; i++ {
		if err := p.Publish(ctx, seqEvent(fmt.Sprintf("filler-%d", i))); err != nil {
			t.Fatalf("filler %d: %v", i, err)
		}
	}

	p.seqMu.Lock()
	_, still := p.sequence[seqKey{"observability.decision.logged", "order-idle"}]
	p.seqMu.Unlock()
	if still {
		t.Fatal("the idle key survived both the TTL and a sweep at capacity — nothing was evicted, " +
			"so the bound above is being met by luck rather than by the gc")
	}

	// It publishes again, and Validate accepts it. This is the assertion that
	// matters: the bound must not have become a publish failure.
	if err := p.Publish(ctx, seqEvent("order-idle")); err != nil {
		t.Fatalf("republishing an evicted key failed: %v — Validate refuses producer_sequence 0 "+
			"while partition_key is set, so an eviction that returned 0 would take the order path "+
			"down", err)
	}
	p.seqMu.Lock()
	got := p.sequence[seqKey{"observability.decision.logged", "order-idle"}].seq
	p.seqMu.Unlock()
	if got != 1 {
		t.Fatalf("an evicted key resumed at %d, want 1 — the same value a fresh process stamps", got)
	}
}

// THE DEFAULTS APPLY WITHOUT ANY SERVICE CONFIGURING THEM. Twenty-odd
// composition roots build a Producer; a bound that only existed when somebody
// passed it would be a bound almost nothing had.
func TestTheSequenceBoundAppliesWithoutConfiguration(t *testing.T) {
	p, err := NewProducer(&countingClient{}, ProducerConfig{
		Source: "test", ProducerVersion: "v0", Tenant: "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	if p.seqMax <= 0 || p.seqTTL <= 0 {
		t.Fatalf("an unconfigured producer has seqMax=%d seqTTL=%v — the map is unbounded for "+
			"every service that does not opt in, which is all of them", p.seqMax, p.seqTTL)
	}
}
