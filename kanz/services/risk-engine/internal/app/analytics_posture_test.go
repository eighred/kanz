package app

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/eighred/kanz/internal/risk/benchmarks"
	"github.com/eighred/kanz/internal/validation"
)

var postureNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// scrape gathers kanz_risk_analytics_validated into analytic -> (value, state).
func scrape(t *testing.T, reg *prometheus.Registry) map[string]struct {
	value float64
	state string
} {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]struct {
		value float64
		state string
	}{}
	for _, f := range families {
		if f.GetName() != "kanz_risk_analytics_validated" {
			continue
		}
		for _, m := range f.GetMetric() {
			var analytic, state string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "analytic":
					analytic = l.GetValue()
				case "state":
					state = l.GetValue()
				}
			}
			if _, dup := out[analytic]; dup {
				t.Fatalf("%s reported twice in one scrape — an analytic in two states at once "+
					"makes sum() over this metric meaningless", analytic)
			}
			out[analytic] = struct {
				value float64
				state string
			}{m.GetGauge().GetValue(), state}
		}
	}
	return out
}

func loadedGate(t *testing.T, clock func() time.Time) *validation.Gate {
	t.Helper()
	g := validation.NewGate(clock)
	if err := LoadValidations(g, func() time.Time { return postureNow }); err != nil {
		t.Fatalf("LoadValidations: %v", err)
	}
	return g
}

func register(t *testing.T, gate *validation.Gate) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	AnalyticsPosture(reg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), gate)
	return reg
}

// EVERY ANALYTIC IS PRESENT, INCLUDING THE ZEROES. This is the property #471
// turns on: a metric carrying only the validated analytics would report full
// coverage on a platform that had validated two of ten.
func TestAnalyticsPosture_ReportsTheUnvalidatedOnesToo(t *testing.T) {
	got := scrape(t, register(t, loadedGate(t, func() time.Time { return postureNow })))

	inv := benchmarks.Inventory()
	if len(got) != len(inv) {
		t.Fatalf("scraped %d analytics, want %d — the inventory and the metric disagree", len(got), len(inv))
	}
	for _, a := range inv {
		if _, ok := got[a]; !ok {
			t.Errorf("%s is in the inventory but absent from the metric — an analytic nobody has "+
				"benchmarked must still be counted, or the gap is invisible", a)
		}
	}

	// And the count is honestly BELOW full: if this ever equals len(inv) without
	// benchmarks being added, something is vouching for analytics it never graded.
	var validated int
	for _, v := range got {
		if v.value == 1 {
			validated++
		}
	}
	if validated == 0 {
		t.Error("no analytic reads as validated — LoadValidations recorded nothing")
	}
	if validated == len(inv) {
		t.Errorf("all %d analytics read as validated, but only %d have benchmark evidence — "+
			"the gauge is vouching for analytics nobody graded", len(inv), validated)
	}
}

// A ZERO SAYS WHY IT IS A ZERO. "Benchmarked and wrong" and "never benchmarked"
// are different findings, and collapsing them lets a broken analytic hide among
// the unchecked ones.
func TestAnalyticsPosture_DistinguishesAbsentFromValidated(t *testing.T) {
	got := scrape(t, register(t, loadedGate(t, func() time.Time { return postureNow })))

	if v := got[benchmarks.AnalyticBlackScholes]; v.value != 1 || v.state != "validated" {
		t.Errorf("black_scholes = (%v, %q), want (1, validated) — it reproduces Hull's published "+
			"example and holds a signed report", v.value, v.state)
	}
	if v := got["isda_simm"]; v.value != 0 || v.state != "absent" {
		t.Errorf("isda_simm = (%v, %q), want (0, absent) — nothing has ever benchmarked it",
			v.value, v.state)
	}
}

// A FAILING BENCHMARK READS "failed", NOT "absent" — and is still recorded.
//
// A failed validation is signed audit evidence that the analytic was graded and
// did not reproduce its benchmark. Dropping it would make a WRONG analytic
// indistinguishable from an unchecked one, which is the worse of the two to hide.
func TestAnalyticsPosture_AFailedValidationIsVisibleAndDistinct(t *testing.T) {
	gate := validation.NewGate(func() time.Time { return postureNow })
	bad, err := validation.Validate("value_at_risk",
		[]validation.Case{{Name: "kupiec", Got: 10, Want: 1, Tolerance: 0.1}},
		postureNow, 0, validation.HashSigner{})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if bad.Passed {
		t.Fatal("this case set was built to fail")
	}
	if err := gate.Record(bad); err != nil {
		t.Fatalf("a FAILING report must still be recordable as evidence: %v", err)
	}

	got := scrape(t, register(t, gate))
	if v := got["value_at_risk"]; v.value != 0 || v.state != "failed" {
		t.Errorf("value_at_risk = (%v, %q), want (0, failed) — an analytic that was graded and "+
			"got it wrong must not read the same as one nobody graded", v.value, v.state)
	}
}

// THE METRIC GOES STALE BY ITSELF.
//
// This is why the posture is a Collector and not a gauge written once at startup.
// A validation expires after DefaultValidity because SR 11-7 is about CURRENT
// validation; a value set at boot would keep reporting validated=1 for the life
// of the process, sailing past the expiry the gate exists to enforce — and the
// day it started lying, nothing in the output would change.
//
// SAME COLLECTOR, SAME REGISTRY, ONLY THE CLOCK MOVES. Re-registering would prove
// nothing about the one thing at issue: that a long-lived process notices.
func TestAnalyticsPosture_ExpiryIsEvaluatedAtScrapeTimeNotAtStartup(t *testing.T) {
	now := postureNow
	gate := loadedGate(t, func() time.Time { return now })
	reg := register(t, gate)

	if v := scrape(t, reg)[benchmarks.AnalyticBlackScholes]; v.value != 1 {
		t.Fatalf("black_scholes = %v at registration, want 1", v.value)
	}

	now = postureNow.Add(validation.DefaultValidity + time.Hour)

	v := scrape(t, reg)[benchmarks.AnalyticBlackScholes]
	if v.value != 0 || v.state != "expired" {
		t.Fatalf("black_scholes = (%v, %q) after its validation expired, want (0, expired) — the "+
			"gauge is still reporting a sign-off that is no longer current", v.value, v.state)
	}
}

// THE POSTURE DOES NOT REFUSE ANYTHING. #471 slice 3 arms that, with the list in
// hand and on a lead's ruling; this slice counts the gap. If AnalyticsPosture
// ever grows the power to stop a pricing path, it must not do so from a
// constructor that eight of ten analytics currently fail.
func TestAnalyticsPosture_CountsTheGapWithoutRefusing(t *testing.T) {
	// A gate with NOTHING recorded: every analytic unvalidated, the worst case.
	reg := prometheus.NewRegistry()
	var logs bytes.Buffer
	AnalyticsPosture(reg, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		validation.NewGate(func() time.Time { return postureNow }))

	got := scrape(t, reg)
	for a, v := range got {
		if v.value != 0 || v.state != "absent" {
			t.Errorf("%s = (%v, %q) against an empty gate, want (0, absent)", a, v.value, v.state)
		}
	}
	if len(got) != len(benchmarks.Inventory()) {
		t.Errorf("scraped %d analytics against an empty gate, want all %d — the zeroes are the "+
			"whole point", len(got), len(benchmarks.Inventory()))
	}
	// AND IT SAID SO AT WARN. An operator who never reads this line trusts a
	// number nobody checked; Info is where it gets filtered out.
	if !bytes.Contains(logs.Bytes(), []byte("level=WARN")) {
		t.Errorf("a service where NO analytic is validated logged no warning:\n%s", logs.String())
	}
}

// The gauge is declared as a gauge, not a counter — a posture that can go back
// down after a revalidation.
func TestAnalyticsPosture_IsAGauge(t *testing.T) {
	families, err := register(t, loadedGate(t, func() time.Time { return postureNow })).Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "kanz_risk_analytics_validated" {
			if f.GetType() != dto.MetricType_GAUGE {
				t.Errorf("kanz_risk_analytics_validated is a %v, want GAUGE", f.GetType())
			}
			return
		}
	}
	t.Fatal("kanz_risk_analytics_validated was never registered")
}
