package integrity

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// gather returns the registered metric families keyed by name, after the
// caller has exercised the exporter so every series is materialized.
func gather(t *testing.T, reg *prometheus.Registry) map[string][]string {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := make(map[string][]string)
	for _, mf := range mfs {
		// Label set is taken from the first sample of the family.
		var labels []string
		if len(mf.Metric) > 0 {
			for _, lp := range mf.Metric[0].Label {
				labels = append(labels, lp.GetName())
			}
			sort.Strings(labels)
		}
		out[mf.GetName()] = labels
	}
	return out
}

// TestMetricsContract is the DATA-08 guard: the exporter must expose exactly
// these series with exactly these labels, so dashboards and alert rules can't
// silently break under a rename. Every Observe* path is exercised so each
// family appears in the gather.
func TestMetricsContract(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.now = func() time.Time { return time.Unix(100, 0) }

	m.ObserveStaleness(StalenessResult{
		Subject: "market.equity.trade", PartitionKey: "AAPL",
		Level: StalenessStale, Lag: 12 * time.Second, LastEventTime: time.Unix(90, 0),
	})
	m.ObserveGap(GapResult{
		Key:    StreamKey{Source: "md", EventType: "market.equity.trade", PartitionKey: "AAPL"},
		Status: StatusGap, Sequence: 10, Previous: 7, MissingFrom: 8, MissingTo: 9,
	})
	m.ObserveReconcile(ReconcileResult{Status: ReconcileMatched, MatchLatency: 2 * time.Second})
	m.SetReconcilePending(TransportNATS, 3)
	m.ObserveDiscrepancies([]Discrepancy{{SeenOn: TransportNATS, MissingOn: TransportKafka}})
	m.ObserveDrift(DriftResult{Feature: "spread", Metric: MetricPSI, Score: 0.3, Threshold: 0.25})
	m.ObserveQualityEvent("gap", "critical", "market.equity.trade")

	want := map[string][]string{
		"kanz_data_staleness_lag_seconds":           {"partition_key", "subject"},
		"kanz_data_last_event_age_seconds":          {"partition_key", "subject"},
		"kanz_data_gap_missing_total":               {"partition_key", "source", "subject"},
		"kanz_data_reconcile_pending":               {"transport"},
		"kanz_data_reconcile_discrepancies_total":   {"missing_on", "seen_on"},
		"kanz_data_reconcile_match_latency_seconds": nil,
		"kanz_data_drift_score":                     {"feature", "metric"},
		"kanz_data_drift_threshold":                 {"feature", "metric"},
		"kanz_data_quality_events_total":            {"kind", "severity", "subject"},
	}

	got := gather(t, reg)
	for name, wantLabels := range want {
		gotLabels, ok := got[name]
		if !ok {
			t.Errorf("missing series %q", name)
			continue
		}
		if strings.Join(gotLabels, ",") != strings.Join(wantLabels, ",") {
			t.Errorf("%s labels = %v, want %v", name, gotLabels, wantLabels)
		}
	}
	for name := range got {
		if strings.HasPrefix(name, "kanz_data_") {
			if _, expected := want[name]; !expected {
				t.Errorf("unexpected kanz_data_ series %q (contract drift)", name)
			}
		}
	}
}

func TestMetricsValues(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.now = func() time.Time { return time.Unix(100, 0) }

	m.ObserveStaleness(StalenessResult{
		Subject: "s", PartitionKey: "p", Level: StalenessStale,
		Lag: 12 * time.Second, LastEventTime: time.Unix(90, 0),
	})
	if v := readGauge(t, reg, "kanz_data_staleness_lag_seconds"); v != 12 {
		t.Errorf("lag = %v, want 12", v)
	}
	if v := readGauge(t, reg, "kanz_data_last_event_age_seconds"); v != 10 {
		t.Errorf("age = %v, want 10 (100-90)", v)
	}

	// StalenessUnknown has no subject and must not emit an empty-label series.
	m.ObserveStaleness(StalenessResult{Level: StalenessUnknown})
	m.ObserveGap(GapResult{Status: StatusOK}) // non-gap is a no-op
	if v := readCounter(t, reg, "kanz_data_gap_missing_total"); v != 0 {
		t.Errorf("non-gap incremented counter: %v", v)
	}

	m.ObserveGap(GapResult{
		Key:    StreamKey{Source: "md", EventType: "s", PartitionKey: "p"},
		Status: StatusGap, MissingFrom: 8, MissingTo: 9, Sequence: 10,
	})
	if v := readCounter(t, reg, "kanz_data_gap_missing_total"); v != 2 {
		t.Errorf("gap missing = %v, want 2", v)
	}
}

// Gather returns *dto.MetricFamily; these helpers read a single sample's value
// without pulling in testutil.
func readGauge(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	for _, mf := range gatherFamilies(t, reg) {
		if mf.GetName() == name && len(mf.Metric) > 0 {
			return mf.Metric[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("series %q not found", name)
	return 0
}

func readCounter(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	for _, mf := range gatherFamilies(t, reg) {
		if mf.GetName() == name {
			var sum float64
			for _, met := range mf.Metric {
				sum += met.GetCounter().GetValue()
			}
			return sum
		}
	}
	return 0
}

func gatherFamilies(t *testing.T, reg *prometheus.Registry) []*dto.MetricFamily {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	return mfs
}
