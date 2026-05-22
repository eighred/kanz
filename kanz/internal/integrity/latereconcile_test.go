package integrity_test

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/internal/integrity"
)

// DATA-11 — scenario tests for the late-data path (DATA-03 watermark +
// MarkLate flag wiring) and reconciliation correctness (DATA-05) under
// simulated out-of-order and divergent streams. Like DATA-10, these exercise
// the primitives over a realistic event sequence (seeded rand, reproducible)
// rather than one classification call, and the final tests tie the two
// primitives together — the spirit of "late-data path + reconciliation" as
// one task. Reuses the helpers from watermark_test.go (tradeAt/wmBase) and
// reconcile_test.go (recEnv/fakeClock/recBase).

// --- Late-data path -------------------------------------------------------

func TestLate_StragglersFlaggedAndMarkedAfterFrontierAdvances(t *testing.T) {
	w := integrity.NewWatermarkTracker(5 * time.Second)

	// Advance the frontier with 61 in-order events (t = base .. base+60s);
	// watermark ends at base+55s. None are late.
	for i := 0; i <= 60; i++ {
		r := w.Observe(tradeAt(wmBase.Add(time.Duration(i) * time.Second)))
		if r.Late() {
			t.Fatalf("in-order event at +%ds flagged late", i)
		}
	}

	// A burst of stragglers far below the watermark (event_time base+10s).
	// Each is late, gets MarkLate'd, and IsLate must then be observable —
	// the detect → route → payload-blind-tooling-sees-it path (DATA-06).
	const stragglers = 10
	late := 0
	for j := 0; j < stragglers; j++ {
		env := tradeAt(wmBase.Add(10 * time.Second))
		r := w.Observe(env)
		if r.Late() {
			late++
			integrity.MarkLate(env)
		}
		if !integrity.IsLate(env) {
			t.Errorf("straggler %d not observable as late after MarkLate", j)
		}
	}
	if late != stragglers {
		t.Errorf("flagged %d late, want %d", late, stragglers)
	}

	// Stragglers must not have rewound the frontier: a fresh event just past
	// the old max is still on-time (the load-bearing DATA-03 invariant).
	r := w.Observe(tradeAt(wmBase.Add(61 * time.Second)))
	if r.Status != integrity.WatermarkOnTime {
		t.Errorf("post-straggler event Status=%v want on_time (frontier rewound?)", r.Status)
	}
}

func TestLate_WithinGraceReorderingIsNeverLate(t *testing.T) {
	// A jittery feed: baseline rises 10s/step, actual event_time jitters
	// ±3s. Jitter (3s) < allowedLateness (5s), so normal intra-partition
	// reordering must never be flagged late.
	w := integrity.NewWatermarkTracker(5 * time.Second)
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 300; i++ {
		baseline := wmBase.Add(time.Duration(i*10) * time.Second)
		jitter := time.Duration(rng.Intn(7)-3) * time.Second // [-3s, +3s]
		if r := w.Observe(tradeAt(baseline.Add(jitter))); r.Late() {
			t.Fatalf("within-grace reordering at step %d flagged late (lateness %s)", i, r.Lateness)
		}
	}
}

// --- Reconciliation correctness -------------------------------------------

func keys(n int) []string {
	ks := make([]string, n)
	for i := range ks {
		ks[i] = fmt.Sprintf("evt-%04d", i)
	}
	return ks
}

func TestReconcile_BothSidesCompleteShuffledAllMatch(t *testing.T) {
	c := &fakeClock{t: recBase}
	r := integrity.NewReconcilerWithClock(30*time.Second, c.now)
	rng := rand.New(rand.NewSource(11))

	ks := keys(100)
	// NATS delivers first (the live spine leads), in arrival order.
	for _, k := range ks {
		c.advance(time.Millisecond)
		if got := r.Observe(integrity.TransportNATS, recEnv(k)); got.Status != integrity.ReconcilePending {
			t.Fatalf("NATS %s Status=%v want pending", k, got.Status)
		}
	}
	// Kafka delivers the same set, shuffled, still within the deadline.
	kafka := append([]string(nil), ks...)
	rng.Shuffle(len(kafka), func(i, j int) { kafka[i], kafka[j] = kafka[j], kafka[i] })
	matched := 0
	for _, k := range kafka {
		c.advance(time.Millisecond)
		if got := r.Observe(integrity.TransportKafka, recEnv(k)); got.Status == integrity.ReconcileMatched {
			matched++
		}
	}
	if matched != 100 {
		t.Errorf("matched %d want 100", matched)
	}
	if r.PendingCount() != 0 {
		t.Errorf("PendingCount=%d want 0", r.PendingCount())
	}
	c.advance(time.Hour)
	if d := r.Sweep(); len(d) != 0 {
		t.Errorf("Sweep found %d discrepancies on a complete run", len(d))
	}
}

func TestReconcile_KafkaDropsBecomeDiscrepancies(t *testing.T) {
	c := &fakeClock{t: recBase}
	r := integrity.NewReconcilerWithClock(30*time.Second, c.now)
	rng := rand.New(rand.NewSource(13))

	ks := keys(100)
	dropped := map[string]bool{}
	for _, k := range ks {
		r.Observe(integrity.TransportNATS, recEnv(k))
		if rng.Float64() < 0.1 { // Kafka (log of record) never sees ~10%
			dropped[k] = true
			continue
		}
		r.Observe(integrity.TransportKafka, recEnv(k))
	}

	c.advance(31 * time.Second) // age the unmatched past the deadline
	d := r.Sweep()
	if len(d) != len(dropped) {
		t.Fatalf("Sweep found %d discrepancies, %d dropped", len(d), len(dropped))
	}
	for _, disc := range d {
		if !dropped[disc.Key] {
			t.Errorf("discrepancy %q was not dropped", disc.Key)
		}
		if disc.SeenOn != integrity.TransportNATS || disc.MissingOn != integrity.TransportKafka {
			t.Errorf("discrepancy %q SeenOn=%v MissingOn=%v want nats/kafka", disc.Key, disc.SeenOn, disc.MissingOn)
		}
	}
}

func TestReconcile_DurableLogLaggingUnderDeadlineRaisesNoFalseDiscrepancy(t *testing.T) {
	// The realistic healthy case: Kafka lags NATS by a fixed 10s (< 30s
	// deadline). A sweep taken while events are in-flight must not
	// false-positive; every event matches once Kafka catches up.
	c := &fakeClock{t: recBase}
	r := integrity.NewReconcilerWithClock(30*time.Second, c.now)

	ks := keys(50)
	for _, k := range ks {
		r.Observe(integrity.TransportNATS, recEnv(k))
	}
	c.advance(10 * time.Second) // lag, still under the deadline
	if d := r.Sweep(); len(d) != 0 {
		t.Fatalf("Sweep mid-lag found %d discrepancies (deadline not yet reached)", len(d))
	}
	matched := 0
	for _, k := range ks {
		got := r.Observe(integrity.TransportKafka, recEnv(k))
		if got.Status == integrity.ReconcileMatched {
			matched++
			if got.MatchLatency != 10*time.Second {
				t.Errorf("%s MatchLatency=%v want 10s", k, got.MatchLatency)
			}
		}
	}
	if matched != 50 {
		t.Errorf("matched %d want 50", matched)
	}
}

// --- Late + reconciliation interplay --------------------------------------

func TestLateReconcile_LateFlaggedEventStillReconciles(t *testing.T) {
	// Lateness (event-time ordering) and cross-transport completeness are
	// orthogonal: the reconciler keys on idempotency_key and is flag-blind,
	// so an event MarkLate'd on one transport still matches its counterpart.
	c := &fakeClock{t: recBase}
	r := integrity.NewReconcilerWithClock(30*time.Second, c.now)

	natsCopy := recEnv("late-evt-1")
	integrity.MarkLate(natsCopy) // flagged late on the live path
	if got := r.Observe(integrity.TransportNATS, natsCopy); got.Status != integrity.ReconcilePending {
		t.Fatalf("NATS late event Status=%v want pending", got.Status)
	}

	kafkaCopy := recEnv("late-evt-1") // same logical event, no flag yet
	got := r.Observe(integrity.TransportKafka, kafkaCopy)
	if got.Status != integrity.ReconcileMatched {
		t.Errorf("late event did not reconcile: Status=%v", got.Status)
	}
	if !integrity.IsLate(natsCopy) {
		t.Error("reconciliation must not strip the LATE flag from the NATS copy")
	}
}
