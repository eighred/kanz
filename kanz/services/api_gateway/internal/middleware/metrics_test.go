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

// kanz_gateway_oidc_keys_unrevalidated must TRACK the authenticator's state at
// scrape time, not latch a value written once (#242).
//
// This is the assertion that separates the gauge from a counter, and it is the
// whole reason the ruling asked for a gauge: the question during an IdP outage
// is "are we degraded RIGHT NOW", with a fifteen-minute deadline running. A
// value pushed from the request path answers that only while requests keep
// arriving — an outage that also stops traffic would freeze the last write —
// so the collector reads live state on every Gather, and this test Gathers
// three times across a state change to prove it.
//
// It also pins the direction: 0 → 1 → 0. A gauge that rose and never fell would
// leave GatewayOIDCKeysUnrevalidated firing forever, which is how an alerting
// layer trains its readers to ignore it (alerts/README.md).
func TestOIDCKeyGaugeTracksLiveState(t *testing.T) {
	reg := prometheus.NewRegistry()
	degraded := false
	RegisterOIDCKeyGauge(reg, func() bool { return degraded })

	const name = "kanz_gateway_oidc_keys_unrevalidated"
	read := func() float64 {
		t.Helper()
		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		for _, f := range families {
			if f.GetName() != name {
				continue
			}
			if len(f.GetMetric()) != 1 {
				t.Fatalf("%s exported %d series, want 1", name, len(f.GetMetric()))
			}
			return f.GetMetric()[0].GetGauge().GetValue()
		}
		t.Fatalf("%s is not exported at all — an unexported family is an EMPTY vector to "+
			"Prometheus, and an alert over it can never fire", name)
		return 0
	}

	if v := read(); v != 0 {
		t.Errorf("healthy gateway reads %v, want 0", v)
	}
	degraded = true
	if v := read(); v != 1 {
		t.Errorf("degraded gateway reads %v, want 1 — the grace window is invisible, which is the "+
			"half of #242's trade that makes the widened revocation window defensible", v)
	}
	degraded = false
	if v := read(); v != 0 {
		t.Errorf("recovered gateway reads %v, want 0 — a gauge that never falls leaves its alert "+
			"firing forever and the layer gets muted", v)
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
