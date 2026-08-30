package middleware

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
)

// Limits is one tenant's resource budget at the gateway (MT-01e). RatePerSec/
// Burst bound request RATE (token bucket); MaxInFlight bounds CONCURRENCY. A
// non-positive RatePerSec disables rate limiting; a non-positive MaxInFlight
// disables admission control. The two are complementary noisy-neighbor guards:
// rate limiting smooths bursts, admission stops one tenant's slow or heavy
// requests from occupying all gateway capacity and starving others.
type Limits struct {
	RatePerSec  float64 `json:"rate_per_sec"`
	Burst       int     `json:"burst"`
	MaxInFlight int     `json:"max_in_flight"`
}

// TenantLimits is the gateway quota policy: a Default budget every tenant gets,
// plus per-tenant Overrides. The zero value disables both controls.
type TenantLimits struct {
	Default   Limits
	Overrides map[string]Limits
}

func (tl TenantLimits) limit(tenant string) Limits {
	if l, ok := tl.Overrides[tenant]; ok {
		return l
	}
	return tl.Default
}

// LoadQuotaOverrides reads a JSON map of tenant → Limits from path (the
// ConfigMap-mounted policy-as-data shape AUTH-01b/SEC-01d use). Empty path ⇒ no
// overrides. Unknown fields fail loudly so a typo'd budget is caught at boot.
func LoadQuotaOverrides(path string) (map[string]Limits, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var out map[string]Limits
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// QuotaObserver records per-tenant admission outcomes for metrics (MT-01e).
// All methods must be nil-safe at the call site (Quota guards a nil observer).
type QuotaObserver interface {
	RateLimited(tenant string)
	AdmissionRejected(tenant string)
	InFlight(tenant string, delta float64)
}

// Quota enforces per-tenant rate limits AND per-tenant in-flight admission
// (MT-01e noisy-neighbor protection). Buckets/counters are keyed on the
// authenticated tenant (rateKey: tenant → subject → remote addr), so one
// tenant can never starve another. 429 on rate exhaustion, 503 on concurrency
// exhaustion (a transient capacity signal — distinct from 429's "slow down").
// obs may be nil. Runs after Auth so the principal's tenant is on ctx.
func Quota(limits TenantLimits, obs QuotaObserver) func(http.Handler) http.Handler {
	return newQuota(limits, obs).handler()
}

// newQuota builds the state Quota's chain link owns. It is split out so a test
// can drive the WIRED handler and still read the maps behind it — the bound in
// #834 is a property of the state, not of any one response.
func newQuota(limits TenantLimits, obs QuotaObserver) *quota {
	return &quota{
		limits:   limits,
		obs:      obs,
		rates:    newBucketSet(),
		inflight: map[string]int{},
	}
}

func (q *quota) handler() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := rateKey(r)
			tenant := tenantLabel(r)
			lim := q.limits.limit(tenant)

			if !q.allowRate(key, lim) {
				if q.obs != nil {
					q.obs.RateLimited(tenant)
				}
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
			release, ok := q.acquire(key, lim)
			if !ok {
				if q.obs != nil {
					q.obs.AdmissionRejected(tenant)
				}
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusServiceUnavailable, "tenant concurrency limit exceeded")
				return
			}
			if q.obs != nil {
				q.obs.InFlight(tenant, 1)
				defer q.obs.InFlight(tenant, -1)
			}
			defer release()
			next.ServeHTTP(w, r)
		})
	}
}

type quota struct {
	limits TenantLimits
	obs    QuotaObserver
	// rates is the bounded token-bucket store (bucketSet carries its own lock
	// and its own evictor); mu guards inflight alone.
	rates    *bucketSet
	mu       sync.Mutex
	inflight map[string]int
}

func (q *quota) allowRate(key string, lim Limits) bool {
	if lim.RatePerSec <= 0 {
		return true
	}
	burst := float64(lim.Burst)
	if burst < 1 {
		burst = 1
	}
	return q.rates.allow(key, lim.RatePerSec, burst)
}

// acquire reserves an in-flight slot for key, returning a release func and
// whether admission succeeded. When MaxInFlight <= 0 admission is disabled and
// release is a no-op.
//
// # THE KEY IS REMOVED AT ZERO, AND THAT IS THE WHOLE BOUND
//
// A decrement alone left a zero-valued entry behind for every principal the
// gateway had ever served, for the life of the pod (#834). Deleting at zero
// needs no TTL and no ceiling because it makes the invariant structural: a key
// is present only while it holds at least one slot, so len(inflight) can never
// exceed the number of requests actually in flight — a quantity the HTTP server
// already bounds. Nothing here is retained on behalf of history.
func (q *quota) acquire(key string, lim Limits) (func(), bool) {
	if lim.MaxInFlight <= 0 {
		return func() {}, true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.inflight[key] >= lim.MaxInFlight {
		return nil, false
	}
	q.inflight[key]++
	return func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		// <= 1 rather than == 1: a double release must still not underflow, and
		// deleting an already-absent key is a no-op.
		n := q.inflight[key]
		if n <= 1 {
			delete(q.inflight, key)
			return
		}
		q.inflight[key] = n - 1
	}, true
}

// tenantLabel is the metric/quota tenant for a request: the authenticated
// tenant, or "anonymous" when no principal carries one (auth-disabled/dev). It
// is deliberately distinct from observability.SystemTenant — an unauthenticated
// caller is not the platform.
func tenantLabel(r *http.Request) string {
	if p := PrincipalFromContext(r.Context()); p != nil && p.Tenant != "" {
		return p.Tenant
	}
	return "anonymous"
}
