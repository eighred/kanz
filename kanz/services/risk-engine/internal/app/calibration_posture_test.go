package app

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// AN UNSCHEDULED CALIBRATION MUST BE VISIBLE (#113).
//
// The risk-engine implements three calibrations and schedules at most one. Its
// startup line said "calibration scheduler enabled" and named none of the others,
// so two thirds of the platform's calibration was idle and nothing said so — and
// a vol surface nobody refreshes prices an option position exactly like a fresh
// one does.

type postureLogs struct{ b strings.Builder }

func (h *postureLogs) Enabled(context.Context, slog.Level) bool { return true }
func (h *postureLogs) Handle(_ context.Context, r slog.Record) error {
	h.b.WriteString(r.Level.String())
	h.b.WriteString(" ")
	h.b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		h.b.WriteString(" ")
		h.b.WriteString(a.Key)
		h.b.WriteString("=")
		h.b.WriteString(a.Value.String())
		return true
	})
	h.b.WriteString("\n")
	return nil
}
func (h *postureLogs) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *postureLogs) WithGroup(string) slog.Handler      { return h }

func gauge(t *testing.T, reg *prometheus.Registry, kind string) (float64, bool) {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != "kanz_risk_calibration_scheduled" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "kind" && l.GetValue() == kind {
					return m.GetGauge().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// EVERY KIND GETS A SERIES, INCLUDING THE ZEROES.
//
// A metric that appeared only for running calibrations would make an absent one
// indistinguishable from a service that never had the metric at all — which is
// the same "no data reads as fine" failure the posture exists to end.
func TestCalibrationPosture_EveryKindIsReported(t *testing.T) {
	reg := prometheus.NewRegistry()
	logs := &postureLogs{}
	CalibrationPosture(reg, slog.New(logs), map[string]string{"curve": "rates"})

	if v, ok := gauge(t, reg, "curve"); !ok || v != 1 {
		t.Errorf("curve = %v (present=%v), want 1", v, ok)
	}
	for _, kind := range []string{"volsurface", "credit"} {
		v, ok := gauge(t, reg, kind)
		if !ok {
			t.Fatalf("no series for %q — an absent calibration must be a visible ZERO, not a "+
				"missing metric that reads as no data", kind)
		}
		if v != 0 {
			t.Errorf("%s = %v, want 0", kind, v)
		}
	}
}

// AND IT SAYS SO AT WARN, NAMING WHAT IS IDLE.
//
// Info is where this gets filtered out. "Two of the three calibrations are not
// running" is the sentence an operator needs to have read before trusting a vol
// number.
func TestCalibrationPosture_UnscheduledIsWarnedAndNamed(t *testing.T) {
	logs := &postureLogs{}
	CalibrationPosture(prometheus.NewRegistry(), slog.New(logs), map[string]string{"curve": "rates"})

	out := logs.b.String()
	if !strings.Contains(out, "WARN") {
		t.Errorf("the unscheduled calibrations were not reported at WARN; logged:\n%s", out)
	}
	for _, kind := range []string{"volsurface", "credit"} {
		if !strings.Contains(out, kind) {
			t.Errorf("log does not name the idle calibration %q; logged:\n%s", kind, out)
		}
	}
	// The REASON matters as much as the fact: an operator who cannot tell
	// "deliberately unconfigured" from "broken" will treat both as noise.
	if !strings.Contains(out, "quote source") {
		t.Errorf("log does not say WHY they are idle; logged:\n%s", out)
	}
}

// NOTHING SCHEDULED IS THE LOUDEST CASE, and it was previously the quietest: with
// calibration disabled the service said nothing at all, so "no curve either" was
// indistinguishable from a healthy one.
func TestCalibrationPosture_NothingScheduledIsStated(t *testing.T) {
	reg := prometheus.NewRegistry()
	logs := &postureLogs{}
	CalibrationPosture(reg, slog.New(logs), nil)

	for _, kind := range CalibrationKinds {
		if v, ok := gauge(t, reg, kind); !ok || v != 0 {
			t.Errorf("%s = %v (present=%v), want 0", kind, v, ok)
		}
	}
	if !strings.Contains(logs.b.String(), "WARN") {
		t.Error("a service calibrating NOTHING did not warn — the case that most needs saying")
	}
}

// ALL SCHEDULED IS INFO, NOT WARN. A control that warns on a healthy system is a
// control operators learn to ignore.
func TestCalibrationPosture_AllScheduledIsNotAWarning(t *testing.T) {
	logs := &postureLogs{}
	all := map[string]string{}
	for _, k := range CalibrationKinds {
		all[k] = "configured"
	}
	CalibrationPosture(prometheus.NewRegistry(), slog.New(logs), all)

	if strings.Contains(logs.b.String(), "WARN") {
		t.Errorf("warned while every calibration was running; logged:\n%s", logs.b.String())
	}
}
