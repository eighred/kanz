package app

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
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

// register wires the posture in its ADVISORY posture, which is what the coverage
// and expiry tests below are about. The armed posture has its own tests.
func register(t *testing.T, gate *validation.Gate) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := AnalyticsPosture(reg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), gate, false); err != nil {
		t.Fatalf("advisory AnalyticsPosture must never refuse: %v", err)
	}
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
	// EXEMPT IS ITS OWN STATE, AND ITS VALUE IS STILL ZERO. isda_simm has never
	// been benchmarked — that is unchanged and the gauge must not pretend
	// otherwise — but with the gate armed, `exempt` and `absent` have opposite
	// operational meanings: one is licensed by name, the other stops the engine.
	if v := got["isda_simm"]; v.value != 0 || v.state != "exempt" {
		t.Errorf("isda_simm = (%v, %q), want (0, exempt) — nothing has ever benchmarked it, and "+
			"benchmarks.ValidationExemptions() names it with the licence reason", v.value, v.state)
	}
}

// AN EXEMPTION IS A LICENCE TO SERVE UNVALIDATED, NOT A CLAIM OF COVERAGE.
//
// The one way this whole slice could go wrong is an exemption that reads as a
// pass: 12/12 on the gauge, an auditor told everything is validated, and the one
// analytic nobody can grade hiding inside the number. Its value stays 0.
func TestAnalyticsPosture_AnExemptionDoesNotCountAsCoverage(t *testing.T) {
	got := scrape(t, register(t, loadedGate(t, func() time.Time { return postureNow })))

	var validated int
	for _, v := range got {
		validated += int(v.value)
	}
	if validated != len(benchmarks.Inventory())-len(benchmarks.ValidationExemptions()) {
		t.Errorf("sum(kanz_risk_analytics_validated) = %d over %d analytics with %d exempt — an "+
			"exempt analytic must contribute 0, or a coverage ratio improves by excusing things",
			validated, len(benchmarks.Inventory()), len(benchmarks.ValidationExemptions()))
	}
	if len(benchmarks.ValidationExemptions()) == 0 {
		t.Fatal("no exemptions are declared — this test asserts nothing")
	}
}

// ARMED, AND THE ESTATE AS SHIPPED STARTS. This is the whole ruling on #471: the
// eleven that can be graded are enforced, and isda_simm serves under a named
// exemption rather than blocking the arming indefinitely.
func TestAnalyticsPosture_ArmedGateAdmitsTheShippedEstate(t *testing.T) {
	reg := prometheus.NewRegistry()
	var logs bytes.Buffer
	err := AnalyticsPosture(reg, slog.New(slog.NewTextHandler(&logs, nil)),
		loadedGate(t, func() time.Time { return postureNow }), true)
	if err != nil {
		t.Fatalf("RISK_REQUIRE_VALIDATED_ANALYTICS refused the estate as shipped: %v\n%s", err, logs.String())
	}
	if v := enforcedGauge(t, reg); v != 1 {
		t.Errorf("kanz_risk_analytics_validation_enforced = %v with the gate armed, want 1 — an "+
			"armed gate and an unarmed one must not look the same to a scrape", v)
	}
}

// ARMED, AND AN UNEXEMPTED ANALYTIC LOSES ITS EVIDENCE ⇒ THE ENGINE DOES NOT START.
//
// The refusal must name the analytic AND its state, because absent, failed and
// expired have three different repairs.
func TestAnalyticsPosture_ArmedGateRefusesAnUnexemptedAnalytic(t *testing.T) {
	// A gate holding evidence for everything EXCEPT one analytic — the shape of
	// a branch that inventoried an analytic and forgot the benchmark.
	full := loadedGate(t, func() time.Time { return postureNow })
	partial := validation.NewGate(func() time.Time { return postureNow })
	const dropped = benchmarks.AnalyticFRTBSA
	for _, a := range benchmarks.Inventory() {
		if a == dropped {
			continue
		}
		if r, ok := full.Report(a); ok {
			if err := partial.Record(r); err != nil {
				t.Fatalf("Record %s: %v", a, err)
			}
		}
	}

	var logs bytes.Buffer
	err := AnalyticsPosture(prometheus.NewRegistry(), slog.New(slog.NewTextHandler(&logs, nil)), partial, true)
	if err == nil {
		t.Fatalf("%s holds no validation and the gate is ARMED, yet the engine started — this is "+
			"the refusal #471 exists for", dropped)
	}
	if !errors.Is(err, ErrUnvalidatedAnalytic) {
		t.Errorf("refusal is %v, want it to wrap ErrUnvalidatedAnalytic so a caller can tell it "+
			"from a malformed case set", err)
	}
	if !strings.Contains(err.Error(), dropped) {
		t.Errorf("refusal does not name %s: %v — an operator cannot repair what the message does "+
			"not identify", dropped, err)
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Errorf("refusal does not carry the state: %v — absent, failed and expired need three "+
			"different repairs", err)
	}
	if !bytes.Contains(logs.Bytes(), []byte("level=ERROR")) {
		t.Errorf("the refusal was silent in the log:\n%s", logs.String())
	}
}

// AN EXEMPTED ANALYTIC NEVER REFUSES — that is what the exemption buys, and it
// is the difference between arming at eleven-of-twelve and not arming at all.
func TestAnalyticsPosture_ArmedGateDoesNotRefuseAnExemptedAnalytic(t *testing.T) {
	exempt := benchmarks.ValidationExemptions()
	if len(exempt) == 0 {
		t.Skip("no exemptions declared")
	}
	// An EMPTY gate is the worst case: nothing at all is validated. Every
	// unexempted analytic must be named in the refusal, and no exempted one.
	err := AnalyticsPosture(prometheus.NewRegistry(),
		slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		validation.NewGate(func() time.Time { return postureNow }), true)
	if err == nil {
		t.Fatal("an armed gate with NOTHING validated must refuse")
	}
	for a := range exempt {
		if strings.Contains(err.Error(), a) {
			t.Errorf("%s is a named exemption and the refusal names it anyway: %v — the exemption "+
				"is doing nothing and the gate could never have been armed", a, err)
		}
	}
	for _, a := range benchmarks.Inventory() {
		if _, ok := exempt[a]; ok {
			continue
		}
		if !strings.Contains(err.Error(), a) {
			t.Errorf("%s is unvalidated and unexempted, and the refusal does not name it: %v", a, err)
		}
	}
}

// A COUNT WOULD HAVE ACCEPTED THIS, AND THAT IS WHY THERE ISN'T ONE.
//
// Swap the evidence: grade the exempted analytic and un-grade one that was
// validated. Coverage is still eleven of twelve, so "require 11/12" passes — with
// a DIFFERENT analytic serving unvalidated than the one anybody argued for. The
// named list refuses it.
func TestAnalyticsPosture_ACountWouldAcceptTheWrongEleven(t *testing.T) {
	exempt := benchmarks.ValidationExemptions()
	var exempted string
	for a := range exempt {
		exempted = a
	}
	if exempted == "" {
		t.Skip("no exemptions declared")
	}

	full := loadedGate(t, func() time.Time { return postureNow })
	swapped := validation.NewGate(func() time.Time { return postureNow })
	const dropped = benchmarks.AnalyticBlackScholes
	for _, a := range benchmarks.Inventory() {
		if a == dropped {
			continue
		}
		if r, ok := full.Report(a); ok {
			if err := swapped.Record(r); err != nil {
				t.Fatalf("Record %s: %v", a, err)
			}
		}
	}
	// ...and the exempted one now has evidence, so the COUNT is unchanged.
	stand, err := validation.Validate(exempted,
		[]validation.Case{{Name: "invented", Got: 1, Want: 1, Tolerance: 1e-9}},
		postureNow, 0, validation.HashSigner{})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := swapped.Record(stand); err != nil {
		t.Fatalf("Record %s: %v", exempted, err)
	}

	var logs bytes.Buffer
	perr := AnalyticsPosture(prometheus.NewRegistry(), slog.New(slog.NewTextHandler(&logs, nil)), swapped, true)
	if perr == nil {
		t.Fatalf("%s is unvalidated and the engine started — coverage is still 11 of 12, which is "+
			"exactly the substitution a count threshold cannot see", dropped)
	}
	if !strings.Contains(perr.Error(), dropped) {
		t.Errorf("refusal does not name %s: %v", dropped, perr)
	}
	// AND THE NOW-DEAD EXEMPTION SAID SO, LOUDLY. The build guard is what retires
	// it; this is the composition root noticing for the build that got past one.
	if !bytes.Contains(logs.Bytes(), []byte("exemption is DEAD")) {
		t.Errorf("%s gained evidence and its exemption was not reported as dead:\n%s",
			exempted, logs.String())
	}
}

// THE ADVISORY POSTURE IS DISTINGUISHABLE FROM THE ARMED ONE AT SCRAPE TIME.
//
// A fully validated estate emits an identical kanz_risk_analytics_validated under
// both postures. Without this series, "the gate is on" and "the gate was never
// turned on" look the same to everything except a startup log line that is gone
// by the time anyone asks — which is the failure mode #471 is about, one level up.
func TestAnalyticsPosture_EnforcementIsVisibleToAScrape(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := AnalyticsPosture(reg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		loadedGate(t, func() time.Time { return postureNow }), false); err != nil {
		t.Fatal(err)
	}
	if v := enforcedGauge(t, reg); v != 0 {
		t.Errorf("kanz_risk_analytics_validation_enforced = %v with the gate advisory, want 0", v)
	}
}

// enforcedGauge reads kanz_risk_analytics_validation_enforced, failing if it is
// not registered at all — an absent series is not a zero.
func enforcedGauge(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "kanz_risk_analytics_validation_enforced" {
			continue
		}
		if f.GetType() != dto.MetricType_GAUGE {
			t.Errorf("kanz_risk_analytics_validation_enforced is a %v, want GAUGE", f.GetType())
		}
		return f.GetMetric()[0].GetGauge().GetValue()
	}
	t.Fatal("kanz_risk_analytics_validation_enforced was never registered — an operator cannot " +
		"tell an armed gate from an unarmed one")
	return 0
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

// THE ADVISORY POSTURE STILL REFUSES NOTHING. RISK_REQUIRE_VALIDATED_ANALYTICS
// defaults to armed, but false is a supported posture and it must behave exactly
// as it did before #471 slice 3: count the gap, warn, and serve.
func TestAnalyticsPosture_CountsTheGapWithoutRefusing(t *testing.T) {
	// A gate with NOTHING recorded: every analytic unvalidated, the worst case.
	reg := prometheus.NewRegistry()
	var logs bytes.Buffer
	if err := AnalyticsPosture(reg, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		validation.NewGate(func() time.Time { return postureNow }), false); err != nil {
		t.Fatalf("the ADVISORY posture refused a start: %v", err)
	}

	got := scrape(t, reg)
	exempt := benchmarks.ValidationExemptions()
	for a, v := range got {
		want := "absent"
		if _, ok := exempt[a]; ok {
			want = "exempt"
		}
		if v.value != 0 || v.state != want {
			t.Errorf("%s = (%v, %q) against an empty gate, want (0, %s)", a, v.value, v.state, want)
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
