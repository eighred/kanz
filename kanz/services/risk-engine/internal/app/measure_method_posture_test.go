package app

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/risk/compute"
)

// WHAT SERVES THE MEASURE, NOT WHETHER THE NAME IS REGISTERED (#1037).
//
// kanz_risk_measure_live reads 1 for VaR99 whether the number is historical
// simulation or 0.01 × GrossExposure, so the alert an operator needs — "a mandate
// names a measure served by a placeholder" — could not be written. These tests
// pin the series that makes it writable, and the two ways the gauge has gone
// silent in this repository before: a collector behind a branch, and a state with
// no series at all.

// methodSeries returns every kanz_risk_measure_method series as
// measure|method -> value.
func methodSeries(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "kanz_risk_measure_method" {
			continue
		}
		for _, m := range f.GetMetric() {
			var measure, method string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "measure":
					measure = l.GetValue()
				case "method":
					method = l.GetValue()
				}
			}
			out[measure+"|"+method] = m.GetGauge().GetValue()
		}
	}
	return out
}

// THE SERIES EXISTS BEFORE THE FIRST RECOMPUTE. A gauge whose family is empty
// until traffic arrives makes every alert over it evaluate to no-data on a pod
// that has just started — which is the window an operator most wants covered.
func TestMeasureMethodPosture_SeedsEveryServedMeasure(t *testing.T) {
	reg := prometheus.NewRegistry()
	p := NewMeasureMethodPosture(reg)
	p.Seed(compute.DefaultRegistry())

	got := methodSeries(t, reg)
	for _, name := range compute.DefaultRegistry().Names() {
		key := string(name) + "|" + MethodUnobserved
		if got[key] != 1 {
			t.Errorf("%s = %v (present=%v), want 1 — a served measure with no series reads as "+
				"no-data to every consumer", key, got[key], got[key] != 0)
		}
	}
}

// THE DEFECT, AS A SERIES. The two VaR99 answers must land on different series,
// and the placeholder must be the one an operator can select on.
func TestMeasureMethodPosture_ThePlaceholderAndTheModelAreDifferentSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	p := NewMeasureMethodPosture(reg)
	p.Seed(compute.DefaultRegistry())

	p.Observe("VaR99", "placeholder_1pct_gross")
	got := methodSeries(t, reg)
	if got["VaR99|placeholder_1pct_gross"] != 1 {
		t.Errorf("VaR99|placeholder_1pct_gross = %v, want 1 — an operator cannot see that the "+
			"platform's headline risk number is 1%% of gross", got["VaR99|placeholder_1pct_gross"])
	}
	if got["VaR99|"+MethodUnobserved] != 0 {
		t.Errorf("VaR99|unobserved = %v, want 0 — the superseded series must read false rather "+
			"than disappear, or an alert over it stops evaluating instead of evaluating false",
			got["VaR99|"+MethodUnobserved])
	}

	// The same engine, reconfigured with a market-data DSN, announces the same
	// measure NAME from a different model. That has to be a different series.
	p.Observe("VaR99", "historical_simulation")
	got = methodSeries(t, reg)
	if got["VaR99|historical_simulation"] != 1 || got["VaR99|placeholder_1pct_gross"] != 0 {
		t.Errorf("after switching model: historical=%v placeholder=%v, want 1 and 0",
			got["VaR99|historical_simulation"], got["VaR99|placeholder_1pct_gross"])
	}
}

// AN UNDECLARED MEASURE IS ITS OWN STATE. Every measure published before #1037
// declares no model; rendering that as an empty method label would put it in the
// same bucket as a measure nothing has computed yet, and neither is a claim that
// the number is real.
func TestMeasureMethodPosture_UndeclaredIsNotUnobserved(t *testing.T) {
	reg := prometheus.NewRegistry()
	p := NewMeasureMethodPosture(reg)
	p.Seed(compute.DefaultRegistry())

	p.Observe("DV01", "")
	got := methodSeries(t, reg)
	if got["DV01|"+MethodUndeclared] != 1 {
		t.Errorf("DV01|undeclared = %v, want 1", got["DV01|"+MethodUndeclared])
	}
	if _, ok := got["DV01|"]; ok {
		t.Error("an empty method label was exported — undeclared must be named, not rendered blank")
	}
}
