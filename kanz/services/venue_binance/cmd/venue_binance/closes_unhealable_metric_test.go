package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/eighred/kanz/internal/execution"
)

// AN UNHEALABLE CLOSE IS EXPORTED AT ZERO, BEFORE ANY CLOSE IS DROPPED (#1036).
//
// The In-Flight Certainty watchdog used to drop a close it could not turn into a
// venue question on the SAME Resolve() line as one it had healed against venue
// truth, so a seam that had never once reached the exchange looked exactly like
// one finding nothing wrong. That silence is what hid an empty instrument on
// every close the out-of-process adapter tracked.
//
// So the counter is the alertable half — and an unseeded CounterVec exports NO
// series until its first increment, which reads exactly like a counter nobody
// wired. "No close was ever dropped unqueried" and "no metric" must be different
// answers, or an `== 0` alert is silent in precisely the state it was written for.
//
// It also asserts the collector survives registration at all: MustRegister runs
// inside serve(), which no other test in this package reaches, and a bad name or
// a colliding label panics there and crash-loops the pod.
func TestClosesUnhealableCounterExportsBothReasonsAtZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(closesUnhealable)
	reasons := []string{execution.CloseDropNoInstrument, execution.CloseDropUnmappedSymbol}
	for _, reason := range reasons {
		closesUnhealable.WithLabelValues(reason)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var got *dto.MetricFamily
	for _, f := range families {
		if f.GetName() == "kanz_venue_closes_unhealable_total" {
			got = f
		}
	}
	if got == nil {
		t.Fatal("the unhealable-close counter exports no series before any drop — an operator " +
			"cannot tell \"every in-flight close was resolved against the exchange\" from " +
			"\"nobody wired the metric\"")
	}

	seen := map[string]float64{}
	for _, m := range got.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "reason" {
				seen[l.GetValue()] = m.GetCounter().GetValue()
			}
		}
	}
	for _, reason := range reasons {
		v, ok := seen[reason]
		if !ok {
			t.Errorf("reason %q exports no series at startup", reason)
			continue
		}
		if v != 0 {
			t.Errorf("reason %q starts at %v, want 0", reason, v)
		}
	}
}
