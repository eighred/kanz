package app

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
)

// stripGauge reads one series of a strip-coverage gauge by its labels, and says
// whether the series EXISTS. Existence is half of what these tests assert: a
// GaugeVec exports nothing for a label value never set, and an empty vector is
// indistinguishable from a healthy zero to every query that reads it.
func stripGauge(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
	metric:
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if want, ok := labels[l.GetName()]; ok && want != l.GetValue() {
					continue metric
				}
			}
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

func newCoverage(t *testing.T) (*CalibrationCoverage, *prometheus.Registry, *postureLogs) {
	t.Helper()
	reg := prometheus.NewRegistry()
	logs := &postureLogs{}
	return NewCalibrationCoverage(reg, slog.New(logs)), reg, logs
}

// A SHORT STRIP IS VISIBLE WITHOUT READING THE CURVE (#908).
//
// The issue's own "verified when": a refresh whose strip is short of the
// configured set reports quoted-vs-configured per currency, and the risk-engine
// surfaces it — a warning naming the missing instrument ids AND a coverage
// metric beside the existing calibration ones.
func TestStripCoverage_ShortStripIsCountedAndNamed(t *testing.T) {
	cov, reg, logs := newCoverage(t)
	cov.Observe("USD", curve.StripCoverage{
		Configured: 3, Quoted: 1,
		Missing: []curve.MissingQuote{
			{InstrumentID: "USD-SWAP-5Y", Reason: curve.MissingNoQuote},
			{InstrumentID: "USD-SWAP-10Y", Reason: curve.MissingUnusableMid},
		},
	})

	usd := map[string]string{"currency": "USD"}
	if got, ok := stripGauge(t, reg, "kanz_risk_calibration_strip_configured", usd); !ok || got != 3 {
		t.Errorf("kanz_risk_calibration_strip_configured{currency=USD} = %v (exists=%v), want 3", got, ok)
	}
	if got, ok := stripGauge(t, reg, "kanz_risk_calibration_strip_quoted", usd); !ok || got != 1 {
		t.Errorf("kanz_risk_calibration_strip_quoted{currency=USD} = %v (exists=%v), want 1. This "+
			"gauge below _configured is the whole signal: a curve built from one of three "+
			"instruments is otherwise identical to a complete one", got, ok)
	}
	for reason, want := range map[string]float64{curve.MissingNoQuote: 1, curve.MissingUnusableMid: 1} {
		got, ok := stripGauge(t, reg, "kanz_risk_calibration_strip_missing",
			map[string]string{"currency": "USD", "reason": reason})
		if !ok || got != want {
			t.Errorf("missing{currency=USD,reason=%s} = %v (exists=%v), want %v", reason, got, ok, want)
		}
	}

	out := logs.b.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "SHORT") {
		t.Errorf("a short strip must WARN, not Info — Info is where it gets filtered out. Got:\n%s", out)
	}
	// The ids are the part an operator acts on, and they must be in the log
	// rather than in a metric label.
	for _, id := range []string{"USD-SWAP-5Y", "USD-SWAP-10Y"} {
		if !strings.Contains(out, id) {
			t.Errorf("the warning does not name %s, so nobody can go and look at it:\n%s", id, out)
		}
	}
}

// EVERY REASON GETS A SERIES, INCLUDING THE ZEROES — the same rule
// CalibrationPosture states for every kind. A GaugeVec exports no series for a
// label value never set, so a query for "instruments missing because their
// venue quotes garbage" would come back empty both when none are and when
// nothing reports.
func TestStripCoverage_EveryReasonIsSeeded(t *testing.T) {
	cov, reg, _ := newCoverage(t)
	cov.Observe("USD", curve.StripCoverage{
		Configured: 2, Quoted: 1,
		Missing: []curve.MissingQuote{{InstrumentID: "USD-SWAP-5Y", Reason: curve.MissingNoQuote}},
	})
	for _, reason := range []string{curve.MissingNoQuote, curve.MissingUnusableMid} {
		if _, ok := stripGauge(t, reg, "kanz_risk_calibration_strip_missing",
			map[string]string{"currency": "USD", "reason": reason}); !ok {
			t.Errorf("no series for reason %q — an unoccurred reason must read 0, not empty", reason)
		}
	}
}

// A COMPLETE STRIP IS REPORTED TOO. A signal that appears only when something
// is wrong makes "fine" and "not reporting" the same series.
func TestStripCoverage_ACompleteStripStillReports(t *testing.T) {
	cov, reg, logs := newCoverage(t)
	cov.Observe("EUR", curve.StripCoverage{Configured: 4, Quoted: 4})

	eur := map[string]string{"currency": "EUR"}
	q, ok := stripGauge(t, reg, "kanz_risk_calibration_strip_quoted", eur)
	if !ok || q != 4 {
		t.Fatalf("a healthy currency exports no quoted series (%v, exists=%v) — an operator "+
			"cannot tell it from a currency that stopped calibrating", q, ok)
	}
	c, ok := stripGauge(t, reg, "kanz_risk_calibration_strip_configured", eur)
	if !ok || c != 4 {
		t.Fatalf("configured = %v (exists=%v), want 4", c, ok)
	}
	if got, ok := stripGauge(t, reg, "kanz_risk_calibration_strip_missing",
		map[string]string{"currency": "EUR", "reason": curve.MissingNoQuote}); !ok || got != 0 {
		t.Fatalf("missing{no_quote} = %v (exists=%v), want an explicit 0", got, ok)
	}
	if strings.Contains(logs.b.String(), "WARN") {
		t.Errorf("a complete strip warned:\n%s", logs.b.String())
	}
}

// AN UNREPORTED COVERAGE IS NOT A ZERO. A source that declares no strip cannot
// be short of one; writing Configured = 0 would publish the active claim "this
// curve needs nothing" in place of "the source never said".
func TestStripCoverage_UnreportedIsLoudAndNotAZero(t *testing.T) {
	cov, reg, logs := newCoverage(t)
	cov.Observe("USD", curve.StripCoverage{})

	if _, ok := stripGauge(t, reg, "kanz_risk_calibration_strip_configured",
		map[string]string{"currency": "USD"}); ok {
		t.Error("an unreported coverage was exported as a series — a zero denominator reads as " +
			"a curve that needs no instruments")
	}
	if !strings.Contains(logs.b.String(), "ERROR") {
		t.Errorf("every currency with a job came from the parsed reference spec, so an empty "+
			"strip is a wiring defect and must be loud. Got:\n%s", logs.b.String())
	}
}

// THE WARNING IS LOGGED ON CHANGE, NOT EVERY REFRESH — the intraday cadence is
// minutes and a dead instrument stays dead, so an unconditional line would
// repeat forever and teach its readers to skip the calibration log. The gauges
// carry the standing state.
//
// AND A STRIP THAT DEGRADES FURTHER LOGS AGAIN: the dedupe key is the whole
// state, not "was it short last time".
func TestStripCoverage_LogsOnChangeAndOnEveryDegradation(t *testing.T) {
	cov, _, logs := newCoverage(t)
	short := curve.StripCoverage{
		Configured: 3, Quoted: 2,
		Missing: []curve.MissingQuote{{InstrumentID: "USD-SWAP-5Y", Reason: curve.MissingNoQuote}},
	}
	for range 5 {
		cov.Observe("USD", short)
	}
	if n := strings.Count(logs.b.String(), "SHORT"); n != 1 {
		t.Fatalf("five identical refreshes produced %d warnings, want 1", n)
	}

	worse := curve.StripCoverage{
		Configured: 3, Quoted: 1,
		Missing: []curve.MissingQuote{
			{InstrumentID: "USD-SWAP-5Y", Reason: curve.MissingNoQuote},
			{InstrumentID: "USD-SWAP-2Y", Reason: curve.MissingNoQuote},
		},
	}
	cov.Observe("USD", worse)
	if n := strings.Count(logs.b.String(), "SHORT"); n != 2 {
		t.Fatalf("a strip that lost a SECOND instrument produced %d warnings in total, want 2 — "+
			"the dedupe key must be the state, not \"was it short before\"", n)
	}

	// Recovery is stated too, or the last thing an operator read stays true
	// forever.
	cov.Observe("USD", curve.StripCoverage{Configured: 3, Quoted: 3})
	if !strings.Contains(logs.b.String(), "complete") {
		t.Errorf("a strip that recovered said nothing:\n%s", logs.b.String())
	}
}

// Two currencies do not mask each other: the dedupe is per currency, and one
// healthy strip must not silence a short one.
func TestStripCoverage_CurrenciesAreIndependent(t *testing.T) {
	cov, reg, logs := newCoverage(t)
	cov.Observe("USD", curve.StripCoverage{Configured: 2, Quoted: 2})
	cov.Observe("EUR", curve.StripCoverage{
		Configured: 2, Quoted: 1,
		Missing: []curve.MissingQuote{{InstrumentID: "EUR-SWAP-5Y", Reason: curve.MissingNoQuote}},
	})
	if !strings.Contains(logs.b.String(), "EUR-SWAP-5Y") {
		t.Errorf("a short EUR strip was masked by a healthy USD one:\n%s", logs.b.String())
	}
	if got, _ := stripGauge(t, reg, "kanz_risk_calibration_strip_quoted",
		map[string]string{"currency": "EUR"}); got != 1 {
		t.Errorf("EUR quoted = %v, want 1", got)
	}
}
