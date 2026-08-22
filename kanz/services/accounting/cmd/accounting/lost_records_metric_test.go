package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// THE LOSS COUNTER IS EXPORTED AT ZERO, BEFORE ANYTHING IS LOST (#622).
//
// Seeding matters as much as incrementing. A series that only appears on the
// first loss gives a dashboard nothing to draw and an alert nothing to evaluate
// until the thing it warns about has already happened — and until then it reads
// exactly like a metric nobody wired. "None lost" and "no metric" must be
// different answers. A plain Counter satisfies this by construction the moment it
// is registered, which is why this asserts the value and not merely the name.
//
// It also asserts the collector survives registration. MustRegister runs inside
// the serve path, after the store and broker are dialled, so no other test here
// reaches that line — and a bad metric name panics there, crash-looping the pod
// with a config nobody changed.
func TestAnnouncementsLostExportsZeroBeforeAnyLoss(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(announcementsLost)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var got *dto.MetricFamily
	for _, f := range families {
		if f.GetName() == "kanz_accounting_cash_announcements_lost_total" {
			got = f
		}
	}
	if got == nil {
		t.Fatal("kanz_accounting_cash_announcements_lost_total exports no series at startup — an operator cannot " +
			"tell \"nothing was lost\" from \"nobody wired the metric\"")
	}
	if len(got.GetMetric()) != 1 {
		t.Fatalf("want exactly one series, got %d", len(got.GetMetric()))
	}
	if v := got.GetMetric()[0].GetCounter().GetValue(); v != 0 {
		t.Fatalf("counter starts at %v, want 0", v)
	}
}
