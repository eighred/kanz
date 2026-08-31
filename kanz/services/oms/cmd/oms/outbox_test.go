package main

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// THE OUTBOX'S THREE SIGNALS, WHICH NOTHING EXERCISED (#292, #643).
//
// The relay has real tests in internal/outbox. What had none is the COMPOSITION:
// whether the counters this process registers are the ones the relay is given,
// and whether the depth gauge reads the live relay rather than a snapshot taken
// at startup. Both are wiring, both fail silently, and the second one fails in
// the direction that reports health — a gauge frozen at zero says "drained" for a
// relay that died an hour ago.

// stubRelay reports a depth this test controls, so the gauge can be proven to
// re-read rather than to have captured a value.
type stubRelay struct {
	age   float64
	reads int
}

func (s *stubRelay) OldestPendingAge(context.Context) float64 {
	s.reads++
	return s.age
}

// BOTH COUNTERS EXIST BEFORE ANYTHING CAN INCREMENT THEM. A counter that appears
// only on its first increment reads as no-data to an alert, so the alert cannot
// fire on the transition from none to some — which is the transition that says
// the relay started failing.
func TestBothOutboxCountersAreRegisteredAtZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	opts := buildOutboxRelayOptions(reg, time.Second)
	if len(opts) != 2 {
		t.Fatalf("buildOutboxRelayOptions returned %d options, want the interval and the counters — "+
			"a relay built without the counters publishes with nothing counting", len(opts))
	}
	for _, name := range []string{
		"kanz_oms_outbox_published_total",
		"kanz_oms_outbox_publish_failures_total",
	} {
		// GATHERED, not just built: a counter the process never registered is
		// absent from /metrics, and absent is no-data rather than zero — an alert
		// cannot fire on the transition from none to some, which is the transition
		// that says the relay started failing.
		if got := testutil.CollectAndCount(reg, name); got != 1 {
			t.Errorf("%s has %d series in the registry, want 1 — it was built and never registered",
				name, got)
		}
	}
}

// A COUNTER REGISTERED AND NEVER HANDED TO THE RELAY IS INVISIBLE — the relay
// works, the dashboard reads zero for ever, and "the relay has stopped" and
// "nobody is counting" look identical. That property is held by test/arch's
// TestEveryRegisteredMetricHasAWriter rather than here, and it can only see it
// because buildOutboxRelayOptions keeps the construction and the hand-off on one
// identifier in one scope; see that function's doc for the two shapes that
// defeated it.

// THE DEPTH GAUGE READS THE RELAY EVERY TIME IT IS SCRAPED. Captured once at
// startup it would report the depth at boot — which is zero on every pod, for
// ever, including the pod whose relay is dead.
func TestTheDepthGaugeIsReadLiveRatherThanCaptured(t *testing.T) {
	reg := prometheus.NewRegistry()
	relay := &stubRelay{age: 0}
	announceOutboxDepth(context.Background(), relay, reg)

	if got := gaugeValue(t, reg, "kanz_oms_outbox_oldest_pending_seconds"); got != 0 {
		t.Fatalf("depth = %v on a drained outbox, want 0", got)
	}
	relay.age = 42
	got := gaugeValue(t, reg, "kanz_oms_outbox_oldest_pending_seconds")
	if got != 42 {
		t.Errorf("depth = %v after the relay fell behind, want 42 — the gauge captured a value at "+
			"registration, so a stalled relay reports the depth it had at boot: zero, healthy, "+
			"and wrong", got)
	}
	if relay.reads < 2 {
		t.Errorf("the relay was read %d times across two scrapes — the gauge is not a live read",
			relay.reads)
	}
}

// --- helpers --------------------------------------------------------------

// gaugeValue reads one unlabelled process-wide gauge out of a registry.
//
// EXACTLY ONE SERIES, NOT THE FIRST OF SEVERAL. Ranging and returning the first
// metric made the helper answer for a LabelVec by picking whichever series the
// gather happened to order first — a caller asserting a total would then be
// reading one arbitrary label's value and passing. Every gauge read through this
// helper is unlabelled, so the plural case is a mistake rather than a shape to
// support, and it fails here instead of silently narrowing the assertion.
//
// ABSENCE FAILS LOUDLY AND NEVER READS AS ZERO. Alert rules in
// infra/observability/alerts/ name these metrics, and a rule over a series with
// no producer yields an empty vector and can never fire — which reads as a
// healthy platform (alerts/README.md). A helper that returned 0 for "not
// registered" would let a test assert exactly the value an unregistered metric
// produces.
func gaugeValue(t *testing.T, g prometheus.Gatherer, name string) float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		if len(f.GetMetric()) != 1 {
			t.Fatalf("%s has %d series, want exactly 1 — it is an unlabelled process-wide gauge, "+
				"and reading one of several would narrow the assertion without saying so",
				name, len(f.GetMetric()))
		}
		return f.GetMetric()[0].GetGauge().GetValue()
	}
	t.Fatalf("metric %q is not registered.\n\n"+
		"An alert rule may name it (infra/observability/alerts/operational.rules.yaml). A rule over "+
		"a series with no producer yields an EMPTY VECTOR and can never fire, which reads as a "+
		"healthy platform rather than a broken one — see alerts/README.md.", name)
	return math.NaN()
}
