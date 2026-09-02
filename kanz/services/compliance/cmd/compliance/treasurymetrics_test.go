package main

// Cash-drag metric tests (#963).
//
// Values are asserted as DELTAS around each action, not as absolutes. The
// collectors are package-level, so they carry state between tests in this
// package and an absolute assertion would depend on test ordering — which is the
// kind of green that stops meaning anything. Seeding is asserted by label
// PRESENCE, which is the property that actually matters: a label absent from the
// registry is a series PromQL cannot see at all.

import (
	"bytes"
	"log/slog"
	"math/big"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/eighred/kanz/internal/treasury"
)

// gather returns label-set → value for one family, plus a histogram's count.
func gather(t *testing.T, g prometheus.Gatherer, name string) map[string]float64 {
	t.Helper()
	fams, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			var parts []string
			for _, l := range m.GetLabel() {
				parts = append(parts, l.GetName()+"="+l.GetValue())
			}
			out[strings.Join(parts, ",")] = metricVal(m)
		}
	}
	return out
}

func metricVal(m *dto.Metric) float64 {
	switch {
	case m.GetCounter() != nil:
		return m.GetCounter().GetValue()
	case m.GetHistogram() != nil:
		return float64(m.GetHistogram().GetSampleCount())
	}
	return 0
}

func newTreasuryRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	registerTreasuryMetrics(reg)
	return reg
}

// EVERY REFUSAL REASON EXISTS AS A SERIES BEFORE THE FIRST BOOK IS EVALUATED.
//
// A counter with no series is not zero to PromQL, it is nothing, and increase()
// over nothing is an empty vector no threshold exceeds. TreasuryCashDragNotObserved
// is an `== 0` rule, which is the arm that fails hardest on an absent series: it
// would be silent in exactly the state it exists to catch.
func TestEveryRefusalReasonIsSeeded(t *testing.T) {
	reg := newTreasuryRegistry(t)
	got := gather(t, reg, "kanz_treasury_drag_unmeasurable_total")

	for _, r := range treasury.Reasons() {
		if _, ok := got["reason="+string(r)]; !ok {
			t.Fatalf("no series for reason=%q at startup (have %v). TreasuryCashDragNotObserved is "+
				"an == 0 rule and evaluates to NOTHING over an absent series, so the state where "+
				"the monitor has stopped evaluating would page nobody (#963).", r, got)
		}
	}
	if len(got) != len(treasury.Reasons()) {
		t.Fatalf("seeded %d series for %d reasons — the seeding loop and treasury.Reasons() have "+
			"drifted", len(got), len(treasury.Reasons()))
	}
}

// THE HISTOGRAM EXISTS AT ZERO TOO. The liveness rule sums its _count with the
// refusal counter, so an absent histogram makes that sum an empty vector and the
// == 0 arm silent.
func TestTheShareHistogramIsRegisteredBeforeAnyMeasurement(t *testing.T) {
	reg := newTreasuryRegistry(t)
	if got := gather(t, reg, "kanz_treasury_idle_cash_share"); len(got) != 1 {
		t.Fatalf("the idle-share histogram is not registered at startup (%v). Its _count is half "+
			"of TreasuryCashDragNotObserved's sum.", got)
	}
}

// A REFUSAL IS COUNTED AND IS NOT OBSERVED INTO THE HISTOGRAM.
//
// treasury.Drag.Percent returns 0 on a refusal. Observing that would put every
// unmeasurable book in the lowest bucket and render an estate that can see
// NOTHING as an estate that is fully invested — wrong, and in the flattering
// direction, so nothing downstream would ever question it.
func TestARefusalIsCountedAndNeverObservedAsZero(t *testing.T) {
	reg := newTreasuryRegistry(t)
	obs := cashDragObserver(nil, newOnceSet().first)

	before := gather(t, reg, "kanz_treasury_idle_cash_share")[""]
	obs("acme", "pf-1", treasury.Drag{Reason: treasury.ReasonCashUnvouched})
	after := gather(t, reg, "kanz_treasury_idle_cash_share")[""]

	if after != before {
		t.Fatalf("an UNMEASURABLE book was observed into the idle-share histogram (count %v → %v). "+
			"Drag.Percent is 0 on a refusal, so this reports a portfolio whose cash nobody can "+
			"vouch for as holding no idle cash — the false zero this whole measurement is "+
			"arranged against (#963).", before, after)
	}
	if got := gather(t, reg, "kanz_treasury_drag_unmeasurable_total")["reason=cash_unvouched"]; got < 1 {
		t.Fatalf("the refusal was not counted (reason=cash_unvouched = %v)", got)
	}
}

// A MEASURED DRAG REACHES THE HISTOGRAM AND NOT THE REFUSAL COUNTER.
func TestAMeasuredDragIsObservedOnce(t *testing.T) {
	reg := newTreasuryRegistry(t)
	obs := cashDragObserver(nil, newOnceSet().first)

	beforeH := gather(t, reg, "kanz_treasury_idle_cash_share")[""]
	beforeR := gather(t, reg, "kanz_treasury_drag_unmeasurable_total")["reason=cash_unvouched"]

	obs("acme", "pf-1", treasury.Drag{Status: treasury.StatusMeasured, IdleShare: big.NewRat(1, 20)})

	if got := gather(t, reg, "kanz_treasury_idle_cash_share")[""]; got != beforeH+1 {
		t.Fatalf("histogram count %v → %v, want one observation", beforeH, got)
	}
	if got := gather(t, reg, "kanz_treasury_drag_unmeasurable_total")["reason=cash_unvouched"]; got != beforeR {
		t.Fatalf("a MEASURED drag moved the refusal counter (%v → %v)", beforeR, got)
	}
}

// THE PER-PORTFOLIO WARNING IS LOGGED ONCE, NOT ON EVERY SWEEP.
//
// The monitor re-runs every book it holds on an interval, so a warning per
// evaluation would repeat the same finding for the same portfolio forever and
// bury every other line in the log. The COUNTER still moves each time — that is
// the number that belongs on a dashboard.
func TestThePortfolioWarningIsLoggedOncePerReason(t *testing.T) {
	reg := newTreasuryRegistry(t)
	var buf bytes.Buffer
	obs := cashDragObserver(slog.New(slog.NewTextHandler(&buf, nil)), newOnceSet().first)

	before := gather(t, reg, "kanz_treasury_drag_unmeasurable_total")["reason=cash_unknown"]
	d := treasury.Drag{Reason: treasury.ReasonCashUnknown, Detail: "no announcement"}
	for range 5 {
		obs("acme", "pf-1", d)
	}

	if n := strings.Count(buf.String(), "CANNOT MEASURE"); n != 1 {
		t.Fatalf("logged the same portfolio %d times, want 1 — the monitor sweeps every book on an "+
			"interval, so this line would repeat forever", n)
	}
	if got := gather(t, reg, "kanz_treasury_drag_unmeasurable_total")["reason=cash_unknown"]; got != before+5 {
		t.Fatalf("counter %v → %v, want +5: the LOG is rate-limited and the COUNTER must not be, "+
			"or the dashboard under-reports how much of the book is unmeasurable", before, got)
	}
}

// A NEW REASON FOR THE SAME PORTFOLIO IS LOGGED AGAIN. The reason is part of the
// key because a book that stops failing on cash and starts failing on NAV has a
// different problem and a different owner; suppressing the second would report
// the first one forever.
func TestADifferentReasonForTheSamePortfolioIsLoggedAgain(t *testing.T) {
	newTreasuryRegistry(t)
	var buf bytes.Buffer
	obs := cashDragObserver(slog.New(slog.NewTextHandler(&buf, nil)), newOnceSet().first)

	obs("acme", "pf-1", treasury.Drag{Reason: treasury.ReasonCashUnknown})
	obs("acme", "pf-1", treasury.Drag{Reason: treasury.ReasonNAVNotEquity})

	if n := strings.Count(buf.String(), "CANNOT MEASURE"); n != 2 {
		t.Fatalf("logged %d lines for two different failure reasons on one portfolio, want 2 — "+
			"they route to different teams", n)
	}
}

// THE DETAIL REACHES THE LOG. The reason is a routing label; the detail is what
// tells an operator which feed, which instrument, which currency.
func TestTheRefusalDetailReachesTheLog(t *testing.T) {
	newTreasuryRegistry(t)
	var buf bytes.Buffer
	obs := cashDragObserver(slog.New(slog.NewTextHandler(&buf, nil)), newOnceSet().first)

	obs("acme", "pf-1", treasury.Drag{
		Reason: treasury.ReasonNAVUnknown, Detail: "no mark for SOL-USD",
	})
	for _, want := range []string{"no mark for SOL-USD", "pf-1", "acme", "nav_unknown"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("the log line omits %q: %s", want, buf.String())
		}
	}
}

// A NIL LOGGER STILL COUNTS. A deployment that wired no logger must not silently
// stop feeding the metric that every rule reads.
func TestANilLoggerStillCounts(t *testing.T) {
	reg := newTreasuryRegistry(t)
	obs := cashDragObserver(nil, nil)

	before := gather(t, reg, "kanz_treasury_drag_unmeasurable_total")["reason=nav_not_positive"]
	obs("acme", "pf-1", treasury.Drag{Reason: treasury.ReasonNAVNotPositive})
	if got := gather(t, reg, "kanz_treasury_drag_unmeasurable_total")["reason=nav_not_positive"]; got != before+1 {
		t.Fatalf("counter %v → %v with a nil logger, want +1", before, got)
	}
}
