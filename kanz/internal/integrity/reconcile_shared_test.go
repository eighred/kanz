package integrity_test

import (
	"testing"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/internal/integrity"
)

func envWithKey(key string) *envelopepb.Envelope {
	return &envelopepb.Envelope{IdempotencyKey: key}
}

// DEBT-02b shared-state reconciler mode: two reconciler replicas sharing one
// PendingStore reconcile across instances — a NATS sighting on replica A matches
// the Kafka sighting on replica B, which per-process state cannot do.
func TestSharedStoreReconcilesCrossInstance(t *testing.T) {
	store := integrity.NewInMemoryPendingStore()
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }

	podA := integrity.NewReconcilerWithStore(store, time.Minute, clock)
	podB := integrity.NewReconcilerWithStore(store, time.Minute, clock)

	// Replica A sees the event on the NATS spine first.
	if got := podA.Observe(integrity.TransportNATS, envWithKey("evt-1")); got.Status != integrity.ReconcilePending {
		t.Fatalf("podA NATS: status=%v want pending", got.Status)
	}
	// Replica B sees the SAME event on the Kafka log → matched through the
	// shared store, even though A never saw the Kafka side.
	got := podB.Observe(integrity.TransportKafka, envWithKey("evt-1"))
	if got.Status != integrity.ReconcileMatched {
		t.Fatalf("podB Kafka: status=%v want matched (cross-instance)", got.Status)
	}
	if got.FirstTransport != integrity.TransportNATS {
		t.Errorf("FirstTransport=%v want nats", got.FirstTransport)
	}
	if store.Count() != 0 {
		t.Errorf("pending after match = %d want 0", store.Count())
	}
}

// Contrast: separate per-process stores (the default) cannot reconcile a split
// across replicas — both sides sit Pending forever and would false-positive as
// discrepancies on sweep. Documents why the shared mode exists.
func TestSeparateStoresLeavePendingAcrossInstances(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	podA := integrity.NewReconcilerWithClock(time.Minute, clock) // own store
	podB := integrity.NewReconcilerWithClock(time.Minute, clock) // own store

	podA.Observe(integrity.TransportNATS, envWithKey("evt-1"))
	got := podB.Observe(integrity.TransportKafka, envWithKey("evt-1"))
	if got.Status != integrity.ReconcilePending {
		t.Fatalf("podB with separate store: status=%v want pending (no cross-instance match)", got.Status)
	}
	if podA.PendingCount() != 1 || podB.PendingCount() != 1 {
		t.Errorf("each pod holds its own pending: A=%d B=%d want 1,1", podA.PendingCount(), podB.PendingCount())
	}
}
