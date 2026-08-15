package benchmarks_test

import (
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/benchmarks"
	"github.com/eighred/kanz/internal/validation"
)

// THE POINT OF THESE TESTS IS THAT THE BENCHMARKS CAN FAIL.
//
// A validation suite that passes by construction is a change-detector wearing a
// control's name. What is asserted here is that the analytics REPRODUCE values
// published outside this codebase — so if a pricing path is ever changed in a way
// that breaks Hull's worked example, this is what says so.

var now = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func TestBlackScholesReproducesHullsPublishedExample(t *testing.T) {
	for _, c := range benchmarks.BlackScholes() {
		if !c.Passed() {
			t.Errorf("%s: got %.6f, want %.6f ± %g — the closed-form pricer no longer "+
				"reproduces its independent benchmark", c.Name, c.Got, c.Want, c.Tolerance)
		}
	}
}

func TestBinomialReproducesItsBenchmarks(t *testing.T) {
	for _, c := range benchmarks.Binomial() {
		if !c.Passed() {
			t.Errorf("%s: got %.6f, want %.6f ± %g", c.Name, c.Got, c.Want, c.Tolerance)
		}
	}
}

// EVERY ANALYTIC PRODUCES A SIGNED, PASSING, CURRENT REPORT — which is what the
// gate needs to promote it. A report that validates but does not satisfy the
// gate would leave the analytic unpromotable with nothing saying why.
func TestReportsSatisfyTheGate(t *testing.T) {
	reports, err := benchmarks.Reports(now, nil)
	if err != nil {
		t.Fatalf("Reports: %v", err)
	}
	if len(reports) < 5 {
		t.Fatalf("got %d reports, want one per analytic this package covers (5)", len(reports))
	}

	g := validation.NewGate(func() time.Time { return now })
	for _, r := range reports {
		if r.Signature == "" {
			t.Errorf("%s: report is unsigned — the evidence is not tamper-evident", r.Analytic)
		}
		if err := g.Record(r); err != nil {
			t.Fatalf("%s: Record: %v", r.Analytic, err)
		}
		if err := g.Promote(r.Analytic); err != nil {
			t.Errorf("%s: the gate refuses to promote an analytic that passed its own "+
				"benchmarks: %v", r.Analytic, err)
		}
	}
}

// AND THE GATE STILL DENIES AN ANALYTIC NOBODY BENCHMARKED. Without this, the
// test above is satisfied by a gate that promotes everything — which is the
// state #471 exists to end, reached by a different route.
func TestTheGateStillDeniesAnUnbenchmarkedAnalytic(t *testing.T) {
	reports, err := benchmarks.Reports(now, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := validation.NewGate(func() time.Time { return now })
	for _, r := range reports {
		if err := g.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.Promote("value_at_risk"); err == nil {
		t.Fatal("the gate promoted an analytic with no validation on record — recording some " +
			"analytics must not vouch for the rest")
	}
}

// A VALIDATION EXPIRES. SR 11-7 is about CURRENT validation, not a one-time
// sign-off, and the gate's own doc says so — this pins that the reports this
// package produces are subject to it rather than dated open-endedly.
func TestReportsExpire(t *testing.T) {
	reports, err := benchmarks.Reports(now, nil)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(validation.DefaultValidity + time.Hour)
	g := validation.NewGate(func() time.Time { return later })
	for _, r := range reports {
		if err := g.Record(r); err != nil {
			t.Fatal(err)
		}
		if err := g.Promote(r.Analytic); err == nil {
			t.Errorf("%s: a validation older than its validity still promoted — a stale "+
				"sign-off is what SR 11-7's currency requirement exists to refuse", r.Analytic)
		}
	}
}

// THE VaR BACKTEST REPRODUCES THE BASEL TABLE AND THE PUBLISHED THRESHOLD.
//
// If this fails, the traffic light a supervisor reads is wrong, or the Kupiec
// decision boundary has moved off the chi-squared critical value — either of
// which changes a pass into a fail, or worse, a fail into a pass.
func TestVaRBacktestReproducesItsPublishedBenchmarks(t *testing.T) {
	cases := benchmarks.VaRBacktest()
	if len(cases) < 10 {
		t.Fatalf("only %d backtest cases were built — the fixtures are erroring out silently, and "+
			"a benchmark set that shrinks passes more easily", len(cases))
	}
	for _, c := range cases {
		if !c.Passed() {
			t.Errorf("%s: got %.6f, want %.6f ± %g", c.Name, c.Got, c.Want, c.Tolerance)
		}
	}
}

// THE BOND ANALYTICS REPRODUCE THEIR CLOSED FORMS.
func TestBondAnalyticsReproduceItsPublishedBenchmarks(t *testing.T) {
	cases := benchmarks.BondAnalytics()
	if len(cases) < 8 {
		t.Fatalf("only %d bond cases were built, want at least 8", len(cases))
	}
	for _, c := range cases {
		if !c.Passed() {
			t.Errorf("%s: got %.10f, want %.10f ± %g", c.Name, c.Got, c.Want, c.Tolerance)
		}
	}
}

// THE BUMPED GREEKS ENGINE REPRODUCES THE CLOSED FORM AND ITS IDENTITIES.
//
// It is the sensitivity path for AMERICAN options, where no closed form exists —
// so without this the one Greek path with nothing to check it is also the one
// nobody checked.
func TestGreeksFiniteDifferenceReproducesItsBenchmarks(t *testing.T) {
	cases := benchmarks.GreeksFiniteDifference()
	if len(cases) < 12 {
		t.Fatalf("only %d greek cases were built, want at least 12", len(cases))
	}
	for _, c := range cases {
		if !c.Passed() {
			t.Errorf("%s: got %.9f, want %.9f ± %g", c.Name, c.Got, c.Want, c.Tolerance)
		}
	}
}
