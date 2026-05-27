package middleware

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Every kanz_gateway_* series carries the tenant label (MT-01e), and the
// exporter records request codes + rate-limit rejections per tenant.
func TestGatewayMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewGatewayMetrics(reg)

	// One success then one rate-limited request for the same tenant.
	h := Chain(m.Measure(), Quota(TenantLimits{Default: Limits{RatePerSec: 1, Burst: 1}}, m))(okHandler())
	h.ServeHTTP(httptest.NewRecorder(), reqWithTenant("acme")) // 200
	h.ServeHTTP(httptest.NewRecorder(), reqWithTenant("acme")) // 429

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string]bool{
		"kanz_gateway_requests_total":           false,
		"kanz_gateway_request_duration_seconds": false,
		"kanz_gateway_rate_limited_total":       false,
		"kanz_gateway_inflight_requests":        false,
	}
	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), "kanz_gateway_") {
			continue
		}
		if _, ok := want[f.GetName()]; ok {
			want[f.GetName()] = true
		}
		for _, series := range f.GetMetric() {
			hasTenant := false
			for _, l := range series.GetLabel() {
				if l.GetName() == "tenant" {
					hasTenant = true
				}
			}
			if !hasTenant {
				t.Errorf("%s series missing tenant label", f.GetName())
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("series %s not exported", name)
		}
	}

	if v := testutil.ToFloat64(m.requests.WithLabelValues("acme", "200")); v != 1 {
		t.Errorf("requests_total{acme,200} = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.requests.WithLabelValues("acme", "429")); v != 1 {
		t.Errorf("requests_total{acme,429} = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.rateLimited.WithLabelValues("acme")); v != 1 {
		t.Errorf("rate_limited_total{acme} = %v, want 1", v)
	}
}

// A nil *GatewayMetrics is a safe no-op (un-instrumented gateway).
func TestGatewayMetricsNilSafe(t *testing.T) {
	var m *GatewayMetrics
	h := m.Measure()(okHandler())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, reqWithTenant("acme"))
	m.RateLimited("acme")
	m.AdmissionRejected("acme")
	m.InFlight("acme", 1)
	if rr.Code != 200 {
		t.Errorf("nil-metrics passthrough = %d, want 200", rr.Code)
	}
}
