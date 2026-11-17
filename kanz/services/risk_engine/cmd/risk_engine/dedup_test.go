package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/risk_engine/internal/config"
)

func TestDedupDefaultRetainsPerInstanceWindow(t *testing.T) {
	cfg, err := loadDedupConfig(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	deduper, closer := newDeduper(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), reg)
	if deduper != nil || closer != nil {
		t.Fatal("absent shared selection must retain the consumer's per-instance window")
	}
	assertDedupMetric(t, reg, "kanz_risk_shared_dedup_configured", "", 0)
	for _, op := range []string{"claim", "commit", "release"} {
		assertDedupMetric(t, reg, "kanz_risk_shared_dedup_errors_total", op, 0)
	}
}

func TestDedupMetricsUseClosedOperationLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	onError := sharedDedupMetrics(reg, true)
	onError("claim")
	onError("claim")
	onError("commit")
	onError("release")
	onError("untrusted-backend-response")
	assertDedupMetric(t, reg, "kanz_risk_shared_dedup_configured", "", 1)
	assertDedupMetric(t, reg, "kanz_risk_shared_dedup_errors_total", "claim", 2)
	assertDedupMetric(t, reg, "kanz_risk_shared_dedup_errors_total", "commit", 1)
	assertDedupMetric(t, reg, "kanz_risk_shared_dedup_errors_total", "release", 1)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "kanz_risk_shared_dedup_errors_total" && len(family.Metric) != 3 {
			t.Fatal("backend text created an unbounded metric label")
		}
	}
}

func assertDedupMetric(t *testing.T, reg *prometheus.Registry, name, operation string, want float64) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			label := ""
			for _, item := range metric.Label {
				if item.GetName() == "operation" {
					label = item.GetValue()
				}
			}
			if label == operation {
				got := metric.GetGauge().GetValue() + metric.GetCounter().GetValue()
				if got != want {
					t.Fatalf("%s[%s] = %v, want %v", name, operation, got, want)
				}
				return
			}
		}
	}
	t.Fatalf("missing %s[%s]", name, operation)
}
