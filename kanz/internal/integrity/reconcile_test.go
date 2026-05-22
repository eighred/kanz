package integrity_test

import (
	"testing"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/internal/integrity"
)

func recEnv(idempotencyKey string) *envelopepb.Envelope {
	return &envelopepb.Envelope{IdempotencyKey: idempotencyKey}
}

// fakeClock is a settable clock for deterministic match-deadline tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

var recBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// --- Matching --------------------------------------------------------------

func TestReconcile_NATSThenKafkaMatches(t *testing.T) {
	r := integrity.NewReconciler(30 * time.Second)

	first := r.Observe(integrity.TransportNATS, recEnv("k1"))
	if first.Status != integrity.ReconcilePending {
		t.Fatalf("first Status=%v want pending", first.Status)
	}
	if r.PendingCount() != 1 {
		t.Fatalf("PendingCount=%d want 1", r.PendingCount())
	}

	second := r.Observe(integrity.TransportKafka, recEnv("k1"))
	if second.Status != integrity.ReconcileMatched {
		t.Fatalf("second Status=%v want matched", second.Status)
	}
	if second.FirstTransport != integrity.TransportNATS {
		t.Errorf("FirstTransport=%v want nats", second.FirstTransport)
	}
	if r.PendingCount() != 0 {
		t.Errorf("PendingCount=%d want 0 after match", r.PendingCount())
	}
}

func TestReconcile_KafkaFirstMatchesToo(t *testing.T) {
	r := integrity.NewReconciler(30 * time.Second)
	r.Observe(integrity.TransportKafka, recEnv("k1"))
	res := r.Observe(integrity.TransportNATS, recEnv("k1"))
	if res.Status != integrity.ReconcileMatched {
		t.Fatalf("Status=%v want matched", res.Status)
	}
	if res.FirstTransport != integrity.TransportKafka {
		t.Errorf("FirstTransport=%v want kafka", res.FirstTransport)
	}
}

func TestReconcile_MatchLatencyMeasuredFromFirstSighting(t *testing.T) {
	c := &fakeClock{t: recBase}
	r := integrity.NewReconcilerWithClock(30*time.Second, c.now)

	r.Observe(integrity.TransportNATS, recEnv("k1"))
	c.advance(4 * time.Second)
	res := r.Observe(integrity.TransportKafka, recEnv("k1"))

	if res.MatchLatency != 4*time.Second {
		t.Errorf("MatchLatency=%v want 4s", res.MatchLatency)
	}
}

// --- Duplicates ------------------------------------------------------------

func TestReconcile_SameSideDuplicateBeforeMatch(t *testing.T) {
	r := integrity.NewReconciler(30 * time.Second)
	r.Observe(integrity.TransportNATS, recEnv("k1"))
	dup := r.Observe(integrity.TransportNATS, recEnv("k1"))
	if dup.Status != integrity.ReconcileDuplicate {
		t.Fatalf("Status=%v want duplicate", dup.Status)
	}
	if r.PendingCount() != 1 {
		t.Errorf("PendingCount=%d want 1 (duplicate must not add a second entry)", r.PendingCount())
	}
}

func TestReconcile_DuplicateKeepsOriginalDeadline(t *testing.T) {
	c := &fakeClock{t: recBase}
	r := integrity.NewReconcilerWithClock(30*time.Second, c.now)

	r.Observe(integrity.TransportNATS, recEnv("k1")) // first seen at recBase
	c.advance(20 * time.Second)
	r.Observe(integrity.TransportNATS, recEnv("k1")) // duplicate must not reset firstSeen

	c.advance(11 * time.Second) // now 31s past the original sighting
	d := r.Sweep()
	if len(d) != 1 {
		t.Fatalf("Sweep returned %d discrepancies want 1 (deadline measured from first sight)", len(d))
	}
}

// --- Sweep / discrepancies -------------------------------------------------

func TestReconcile_UnmatchedAgesIntoDiscrepancy(t *testing.T) {
	c := &fakeClock{t: recBase}
	r := integrity.NewReconcilerWithClock(30*time.Second, c.now)

	r.Observe(integrity.TransportNATS, recEnv("k1"))

	// Before the deadline: not yet a discrepancy.
	c.advance(30 * time.Second)
	if d := r.Sweep(); len(d) != 0 {
		t.Fatalf("Sweep at deadline returned %d want 0 (boundary is above the deadline)", len(d))
	}

	// One tick past the deadline.
	c.advance(time.Nanosecond)
	d := r.Sweep()
	if len(d) != 1 {
		t.Fatalf("Sweep past deadline returned %d want 1", len(d))
	}
	if d[0].SeenOn != integrity.TransportNATS || d[0].MissingOn != integrity.TransportKafka {
		t.Errorf("SeenOn=%v MissingOn=%v want nats/kafka", d[0].SeenOn, d[0].MissingOn)
	}
	if d[0].Key != "k1" {
		t.Errorf("Key=%q want k1", d[0].Key)
	}
}

func TestReconcile_SweepReportsDiscrepancyOnce(t *testing.T) {
	c := &fakeClock{t: recBase}
	r := integrity.NewReconcilerWithClock(30*time.Second, c.now)

	r.Observe(integrity.TransportKafka, recEnv("k1"))
	c.advance(31 * time.Second)

	if d := r.Sweep(); len(d) != 1 {
		t.Fatalf("first Sweep returned %d want 1", len(d))
	}
	if d := r.Sweep(); len(d) != 0 {
		t.Fatalf("second Sweep returned %d want 0 (report-once)", len(d))
	}
	if r.PendingCount() != 0 {
		t.Errorf("PendingCount=%d want 0 after sweep", r.PendingCount())
	}
}

func TestReconcile_MatchedBeforeDeadlineNeverSweeps(t *testing.T) {
	c := &fakeClock{t: recBase}
	r := integrity.NewReconcilerWithClock(30*time.Second, c.now)

	r.Observe(integrity.TransportNATS, recEnv("k1"))
	c.advance(5 * time.Second)
	r.Observe(integrity.TransportKafka, recEnv("k1")) // matched

	c.advance(60 * time.Second)
	if d := r.Sweep(); len(d) != 0 {
		t.Errorf("Sweep returned %d want 0 (matched events are gone)", len(d))
	}
}

func TestReconcile_IndependentKeysDoNotInterfere(t *testing.T) {
	c := &fakeClock{t: recBase}
	r := integrity.NewReconcilerWithClock(30*time.Second, c.now)

	r.Observe(integrity.TransportNATS, recEnv("k1"))
	c.advance(31 * time.Second)
	r.Observe(integrity.TransportNATS, recEnv("k2"))  // newer, within its own deadline
	r.Observe(integrity.TransportKafka, recEnv("k2")) // matched

	d := r.Sweep()
	if len(d) != 1 || d[0].Key != "k1" {
		t.Fatalf("Sweep=%v want exactly k1", d)
	}
}

// --- Not-applicable inputs -------------------------------------------------

func TestReconcile_NotApplicableInputs(t *testing.T) {
	r := integrity.NewReconciler(30 * time.Second)

	cases := []struct {
		name      string
		transport integrity.Transport
		env       *envelopepb.Envelope
	}{
		{"nil envelope", integrity.TransportNATS, nil},
		{"empty idempotency_key", integrity.TransportNATS, recEnv("")},
		{"unspecified transport", integrity.TransportUnspecified, recEnv("k1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := r.Observe(tc.transport, tc.env)
			if res.Status != integrity.ReconcileNotApplicable {
				t.Errorf("Status=%v want not_applicable", res.Status)
			}
			if r.PendingCount() != 0 {
				t.Errorf("PendingCount=%d want 0 (no state for not-applicable)", r.PendingCount())
			}
		})
	}
}

// --- Constructor defaults --------------------------------------------------

func TestReconcile_NonPositiveDeadlineFallsBack(t *testing.T) {
	c := &fakeClock{t: recBase}
	r := integrity.NewReconcilerWithClock(0, c.now) // -> DefaultMatchDeadline (30s)

	r.Observe(integrity.TransportNATS, recEnv("k1"))
	c.advance(integrity.DefaultMatchDeadline)
	if d := r.Sweep(); len(d) != 0 {
		t.Fatalf("at default deadline returned %d want 0", len(d))
	}
	c.advance(time.Nanosecond)
	if d := r.Sweep(); len(d) != 1 {
		t.Fatalf("past default deadline returned %d want 1", len(d))
	}
}
