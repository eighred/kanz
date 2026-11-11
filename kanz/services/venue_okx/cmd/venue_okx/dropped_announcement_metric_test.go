package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/eighred/kanz/internal/venueadapter/balancerecon"
)

// THE DROP COUNTER IS EXPORTED AT ZERO, BEFORE ANYTHING DROPS (#622).
//
// Seeding matters as much as incrementing, and it is the half that is easy to
// skip. An unseeded CounterVec exports NO series until its first increment, so a
// dashboard shows nothing and an alert has nothing to evaluate — which reads
// exactly like a counter nobody registered. "No announcements lost" and "no
// metric" must be different answers.
//
// It also asserts the collector survives registration at all. MustRegister runs
// inside serve(), after the exchange and the broker are dialled, so no other test
// in this package reaches that line — and a bad name or a colliding label panics
// there and crash-loops the pod with a config nobody changed.
func TestBalanceDropCounterExportsBothReasonsAtZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(balanceAnnouncementsDropped)
	for _, reason := range []string{balancerecon.DropUndecodable, balancerecon.DropOutOfDomain} {
		balanceAnnouncementsDropped.WithLabelValues(reason)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var got *dto.MetricFamily
	for _, f := range families {
		if f.GetName() == "kanz_venue_okx_balance_announcements_dropped_total" {
			got = f
		}
	}
	if got == nil {
		t.Fatal("the drop counter exports no series before any drop — an operator cannot tell " +
			"\"nothing was lost\" from \"nobody wired the metric\"")
	}

	seen := map[string]float64{}
	for _, m := range got.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "reason" {
				seen[l.GetValue()] = m.GetCounter().GetValue()
			}
		}
	}
	for _, reason := range []string{balancerecon.DropUndecodable, balancerecon.DropOutOfDomain} {
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
