package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func reqWithTenant(tenant string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	r.RemoteAddr = "10.0.0.9:1"
	if tenant != "" {
		r = r.WithContext(WithPrincipal(r.Context(), &Principal{Subject: "u", Tenant: tenant}))
	}
	return r
}

// A per-tenant override clamps one tenant without affecting another on the
// default budget — the noisy-neighbor guarantee.
func TestQuotaPerTenantOverride(t *testing.T) {
	limits := TenantLimits{
		Default:   Limits{RatePerSec: 1000, Burst: 1000},
		Overrides: map[string]Limits{"slow": {RatePerSec: 1, Burst: 1}},
	}
	h := Quota(limits, nil)(okHandler())
	call := func(tenant string) int {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, reqWithTenant(tenant))
		return rr.Code
	}
	if c := call("slow"); c != http.StatusOK {
		t.Fatalf("slow first = %d, want 200", c)
	}
	if c := call("slow"); c != http.StatusTooManyRequests {
		t.Errorf("slow second = %d, want 429 (override exhausted)", c)
	}
	for i := 0; i < 10; i++ {
		if c := call("fast"); c != http.StatusOK {
			t.Fatalf("default-budget tenant throttled at %d: %d", i, c)
		}
	}
}

// Admission caps concurrency: with MaxInFlight=1, a second request arriving
// while the first is still in-flight is rejected 503.
func TestQuotaAdmissionConcurrency(t *testing.T) {
	started := make(chan struct{}, 2) // buffered: the unaffected-tenant call also enters the handler
	release := make(chan struct{})
	blocking := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	h := Quota(TenantLimits{Default: Limits{MaxInFlight: 1}}, nil)(blocking)

	go h.ServeHTTP(httptest.NewRecorder(), reqWithTenant("acme"))
	<-started // first request now holds the only slot

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, reqWithTenant("acme"))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("concurrent request = %d, want 503", rr.Code)
	}
	close(release) // let the first finish

	// A different tenant is unaffected by acme's saturated slot.
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, reqWithTenant("globex"))
	if rr2.Code != http.StatusOK {
		t.Errorf("other tenant starved by acme: %d", rr2.Code)
	}
}

type fakeObs struct {
	rateLimited, admission int
	inflight               float64
}

func (f *fakeObs) RateLimited(string)           { f.rateLimited++ }
func (f *fakeObs) AdmissionRejected(string)     { f.admission++ }
func (f *fakeObs) InFlight(_ string, d float64) { f.inflight += d }

func TestQuotaObserver(t *testing.T) {
	obs := &fakeObs{}
	h := Quota(TenantLimits{Default: Limits{RatePerSec: 1, Burst: 1}}, obs)(okHandler())
	h.ServeHTTP(httptest.NewRecorder(), reqWithTenant("acme")) // 200: inflight +1/-1
	h.ServeHTTP(httptest.NewRecorder(), reqWithTenant("acme")) // 429
	if obs.rateLimited != 1 {
		t.Errorf("RateLimited = %d, want 1", obs.rateLimited)
	}
	if obs.inflight != 0 {
		t.Errorf("InFlight net = %v, want 0 (balanced acquire/release)", obs.inflight)
	}
}

func TestLoadQuotaOverridesEmpty(t *testing.T) {
	m, err := LoadQuotaOverrides("")
	if err != nil || m != nil {
		t.Errorf("empty path = (%v, %v), want (nil, nil)", m, err)
	}
}
