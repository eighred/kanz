package main

import (
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/venueadapter/balancerecon"
)

// THE COMPOSITION ROOT'S BALANCE HALF IS REACHABLE, AND IT IS COMPLETE (#1063).
//
// Everything serve() builds is a local in package main and unreachable by any
// test — which is exactly how OnUnknownBalance stayed nil in both venue adapters
// for a year: the reconcilers honoured the callback, no root supplied one, and a
// nil argument is syntactically identical to a deliberate one. newBalanceSeams
// exists so this half returns what it built and can be asserted about.
//
// The seeding arm is the one an alert depends on: an un-incremented CounterVec
// label exports NO series, so a reason nobody has hit yet reads exactly like a
// counter nobody registered, and `increase(...) > 0` over a family that does not
// exist evaluates against an empty vector.
//
// It also asserts the collectors survive registration at all. MustRegister used
// to run deep inside serve(), after the exchange and the broker are dialled, so
// nothing in this package reached that line — and a bad name or a colliding label
// panics there and crash-loops the pod with a config nobody changed.
func TestBalanceSeamsRegisterAndSeedTheUnknownCounter(t *testing.T) {
	reg := prometheus.NewRegistry()

	seams := newBalanceSeams(reg, slog.Default(), "binance", "acct-1")

	if seams.View == nil {
		t.Fatal("no expected-balance view — reconciliation would compare nothing")
	}
	if seams.Unknown == nil {
		t.Fatal("no unknown-balance observer: an asset this adapter cannot check would be " +
			"skipped in silence, which is indistinguishable from one that reconciled cleanly")
	}

	seen := gatheredReasons(t, reg, balancerecon.UnknownMetricName)
	for _, reason := range execution.BalanceUnknownReasons {
		v, ok := seen[reason]
		if !ok {
			t.Errorf("reason %q exports no series at startup — an alert over it would evaluate "+
				"an empty vector in exactly the state it was written to detect", reason)
			continue
		}
		if v != 0 {
			t.Errorf("reason %q starts at %v, want 0", reason, v)
		}
	}

	// And the observer is bound to the collector this registry holds — not to a
	// second, unregistered one. Without this the seeding above would pass over a
	// counter nothing ever increments.
	seams.Unknown.Observe("USDT", execution.BalanceUnknownNeverAnnounced)
	if got := gatheredReasons(t, reg, balancerecon.UnknownMetricName)[execution.BalanceUnknownNeverAnnounced]; got != 1 {
		t.Fatalf("after one unchecked asset the registered counter reads %v, want 1 — the "+
			"observer is not writing to the collector this root registered", got)
	}
}

func gatheredReasons(t *testing.T, reg *prometheus.Registry, name string) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var got *dto.MetricFamily
	for _, f := range families {
		if f.GetName() == name {
			got = f
		}
	}
	if got == nil {
		t.Fatalf("%s exports no series at all — the collector was not registered", name)
	}
	out := map[string]float64{}
	for _, m := range got.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "reason" {
				out[l.GetValue()] = m.GetCounter().GetValue()
			}
		}
	}
	return out
}
