package venuemargin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/eighred/kanz/internal/execution"
)

func gaugeValue(t *testing.T, g interface{ Write(*dto.Metric) error }) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("gauge write: %v", err)
	}
	return m.GetGauge().GetValue()
}

type nopSource struct{}

func (nopSource) MarginState(context.Context) (execution.VenueMargin, error) {
	return execution.VenueMargin{}, nil
}

// TestAnnounceMakesTheAbsenceVISIBLE is the whole point of the posture gauge:
// "nothing is observing margin here" and "the account is unlevered and safe"
// both produce zero events on the subject, and only this tells them apart.
func TestAnnounceMakesTheAbsenceVisible(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	g := NewGauge("binance")
	if got := Announce(g, logger, "binance", nil); got != nil {
		t.Errorf("Announce(nil) returned %v, want nil", got)
	}
	if v := gaugeValue(t, g); v != 0 {
		t.Errorf("%s = %v with no source, want 0", MetricName, v)
	}
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("no log line for a missing margin source: %v (%q)", err, buf.String())
	}
	if lvl, _ := rec["level"].(string); lvl != "WARN" {
		t.Errorf("log level = %q, want WARN — a silently absent margin seam is the #418 defect again", lvl)
	}
	if msg, _ := rec["msg"].(string); !strings.Contains(msg, Subject) {
		t.Errorf("the warning does not name %s, so an operator cannot connect the silent subject "+
			"to the missing seam: %q", Subject, msg)
	}
}

// TestAnnounceReturnsTheSeamItRecords: returning the value rather than taking a
// pointer is what stops the gauge and the wiring drifting apart — the workers
// receive exactly what was announced.
func TestAnnounceReturnsTheSeamItRecords(t *testing.T) {
	g := NewGauge("okx")
	src := nopSource{}
	got := Announce(g, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), "okx", src)
	if got != execution.VenueMarginSource(src) {
		t.Errorf("Announce returned %v, want the source it was given", got)
	}
	if v := gaugeValue(t, g); v != 1 {
		t.Errorf("%s = %v with a source wired, want 1", MetricName, v)
	}
}
