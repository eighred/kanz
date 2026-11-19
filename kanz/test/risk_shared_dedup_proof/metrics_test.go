package riskdedupproof

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/eighred/kanz/internal/platform/subject"
)

type counters struct {
	handled, suppressed, claimsFailed, recomputed float64
}

func replicaCounters(t *testing.T, ctx context.Context, replica string) counters {
	t.Helper()
	address := "http://127.0.0.1:8081/metrics"
	if replica == "proof-b" {
		address = "http://127.0.0.1:8082/metrics"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	const bound = 2 << 20
	body, err := io.ReadAll(io.LimitReader(res.Body, bound+1))
	if err != nil || len(body) > bound || res.StatusCode != http.StatusOK {
		t.Fatal("metrics response refused")
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	get := func(name string, labels map[string]string) float64 {
		family := families[name]
		for _, metric := range family.GetMetric() {
			if len(metric.Label) != len(labels) {
				continue
			}
			match := true
			for _, label := range metric.Label {
				value, ok := labels[label.GetName()]
				match = match && ok && value == label.GetValue()
			}
			if match {
				switch family.GetType() {
				case dto.MetricType_COUNTER:
					return metric.GetCounter().GetValue()
				case dto.MetricType_GAUGE:
					return metric.GetGauge().GetValue()
				default:
					t.Fatalf("unexpected family type for %s", name)
				}
			}
		}
		return 0
	}
	if get("kanz_risk_shared_dedup_configured", nil) != 1 {
		t.Fatalf("%s did not select shared dedup", replica)
	}
	group := "risk-engine-" + replica
	return counters{
		handled:      get("kanz_bus_consume_total", map[string]string{"subject": subject.PositionAll, "group": group, "result": "ok"}),
		suppressed:   get("kanz_bus_dedup_suppressed_total", map[string]string{"subject": subject.PositionAll, "group": group}),
		claimsFailed: get("kanz_risk_shared_dedup_errors_total", map[string]string{"operation": "claim"}),
		recomputed:   get("kanz_risk_recompute_total", map[string]string{"status": "ok"}),
	}
}

func checkBurst(before, after counters, outage bool) error {
	if outage {
		if after.handled-before.handled != 3 || after.claimsFailed-before.claimsFailed != 3 || after.suppressed != before.suppressed {
			return fmt.Errorf("outage did not deliver all three duplicates through fail-open handling")
		}
	} else if after.suppressed-before.suppressed != 3 || before.handled != after.handled || before.claimsFailed != after.claimsFailed || before.recomputed != after.recomputed {
		return fmt.Errorf("healthy shared dedup did not suppress all three duplicate dispatches")
	}
	return nil
}

func TestProofDuplicateCounters(t *testing.T) {
	before := counters{handled: 2, suppressed: 1, claimsFailed: 4, recomputed: 7}
	for _, tc := range []struct {
		name             string
		after            counters
		outage, accepted bool
	}{
		{"suppressed", counters{2, 4, 4, 7}, false, true},
		{"incomplete", counters{2, 3, 4, 7}, false, false},
		{"extra dispatch", counters{3, 4, 4, 7}, false, false},
		{"unexpected recompute", counters{2, 4, 4, 8}, false, false},
		{"backend unexpectedly failed", counters{2, 4, 5, 7}, false, false},
		{"fail open", counters{5, 1, 7, 8}, true, true},
		{"outage dropped a delivery", counters{4, 1, 7, 7}, true, false},
		{"backend did not fail", counters{5, 1, 4, 7}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if (checkBurst(before, tc.after, tc.outage) == nil) != tc.accepted {
				t.Fatal("unexpected counter verdict")
			}
		})
	}
}
