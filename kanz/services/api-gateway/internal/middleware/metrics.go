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

// RegisterOIDCKeyGauge exports kanz_gateway_oidc_keys_unrevalidated on reg: 1
// while the gateway is verifying bearer tokens against OIDC signing keys it is
// past MaxKeyAge on and could not refetch, 0 otherwise (#242).
//
// WHY THE GAUGE EXISTS AT ALL. pkg/auth now survives an unreachable IdP for a
// bounded KeyGracePeriod instead of failing every request the moment a 5-minute
// key cache expires — which deliberately widens the revocation window from 5
// minutes to 20. A widening bought on purpose is defensible only if the estate
// can SEE when it is being spent, so this is the other half of that trade, not
// a nice-to-have: without it the degraded posture is inferable only from the
// absence of something, and fifteen minutes later authentication stops.
//
// A GAUGE, NOT A COUNTER, AND A GaugeFunc RATHER THAN A PUSHED ONE. A counter
// incremented on entering the grace answers "did this happen", and the question
// with a deadline running is "is it happening NOW". A gauge pushed from the
// request path answers that only while requests keep arriving: an IdP outage
// that also stops traffic would freeze the last value written, which is the
// exact reading an operator must not be given. The closure is evaluated at
// SCRAPE time against live authenticator state, so it cannot go stale and it is
// its own writer (test/arch/metric_writer_test.go).
//
// SEPARATE FROM NewGatewayMetrics ON PURPOSE. Every other kanz_gateway_* series
// is per-tenant RED/quota data; this one is a process-wide posture with no
// tenant to label it by, and it exists only on the OIDC arm. The HS256 dev arm
// has no identity provider to be unreachable, so no series is exported there at
// all — GatewayOIDCKeysUnrevalidated compares against an empty vector and stays
// quiet, which is the truth rather than a reassuring zero.
func RegisterOIDCKeyGauge(reg prometheus.Registerer, unrevalidated func() bool) {
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_gateway_oidc_keys_unrevalidated",
		Help: "1 while the gateway is verifying tokens against OIDC signing keys past MaxKeyAge that it could not refetch (grace window, then fail-closed).",
	}, func() float64 {
		if unrevalidated() {
			return 1
		}
		return 0
	}))
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
