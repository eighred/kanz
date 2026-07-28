package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/pkg/observability"
)

// GatewayMetrics is the api-gateway RED + per-tenant quota exporter (MT-01e).
// Every series carries the platform `tenant` label (observability.TenantLabel)
// so a noisy-neighbor view composes `by (tenant)`. It implements QuotaObserver,
// so the Quota middleware feeds rate-limit/admission/in-flight signals here.
// Register once on the OBS-01a Provider.Registry. A nil *GatewayMetrics is safe
// on every method, so a gateway with no metrics wired is a no-op.
type GatewayMetrics struct {
	requests      *prometheus.CounterVec   // tenant, code
	duration      *prometheus.HistogramVec // tenant
	rateLimited   *prometheus.CounterVec   // tenant
	admissionDrop *prometheus.CounterVec   // tenant
	inflight      *prometheus.GaugeVec     // tenant
}

// NewGatewayMetrics builds and registers the gateway collectors on reg.
func NewGatewayMetrics(reg prometheus.Registerer) *GatewayMetrics {
	t := []string{observability.TenantLabel}
	m := &GatewayMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_gateway_requests_total",
			Help: "gateway requests by tenant and HTTP status code.",
		}, []string{observability.TenantLabel, "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kanz_gateway_request_duration_seconds",
			Help:    "gateway request latency by tenant.",
			Buckets: prometheus.DefBuckets,
		}, t),
		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_gateway_rate_limited_total",
			Help: "requests rejected by the per-tenant rate limit (429).",
		}, t),
		admissionDrop: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_gateway_admission_rejected_total",
			Help: "requests rejected by per-tenant in-flight admission (503).",
		}, t),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_gateway_inflight_requests",
			Help: "in-flight admitted requests by tenant.",
		}, t),
	}
	reg.MustRegister(m.requests, m.duration, m.rateLimited, m.admissionDrop, m.inflight)
	return m
}

// Measure wraps the chain to record per-tenant request count + latency,
// including requests the Quota middleware rejects (their 429/503 codes). Place
// it just after Auth (tenant known) and outside Quota. A nil receiver is a
// pass-through.
func (m *GatewayMetrics) Measure() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if m == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
			next.ServeHTTP(sw, r)
			tenant := tenantLabel(r)
			m.requests.WithLabelValues(tenant, strconv.Itoa(sw.code)).Inc()
			m.duration.WithLabelValues(tenant).Observe(time.Since(start).Seconds())
		})
	}
}

func (m *GatewayMetrics) RateLimited(tenant string) {
	if m != nil {
		m.rateLimited.WithLabelValues(tenant).Inc()
	}
}

func (m *GatewayMetrics) AdmissionRejected(tenant string) {
	if m != nil {
		m.admissionDrop.WithLabelValues(tenant).Inc()
	}
}

func (m *GatewayMetrics) InFlight(tenant string, delta float64) {
	if m != nil {
		m.inflight.WithLabelValues(tenant).Add(delta)
	}
}

var _ QuotaObserver = (*GatewayMetrics)(nil)

// statusWriter captures the response status for the requests_total label
// without buffering the body (unlike recorder, which Idempotency needs).
type statusWriter struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.code = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wrote = true // implicit 200
	return s.ResponseWriter.Write(b)
}
