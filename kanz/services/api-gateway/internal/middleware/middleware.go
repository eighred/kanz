package middleware

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"sync"
	"time"
)

// Chain composes middleware so the FIRST argument is the OUTERMOST decorator
// (runs first on the way in). Chain(a, b, c)(h) == a(b(c(h))).
func Chain(mw ...func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		for i := len(mw) - 1; i >= 0; i-- {
			if mw[i] != nil {
				h = mw[i](h)
			}
		}
		return h
	}
}

// SupportedVersion is the single API version the gateway serves today. The
// REST surface is mounted under /v1; version negotiation rejects an explicit
// request for any other version so a client pinning v2 fails loudly instead of
// silently getting v1 semantics.
const SupportedVersion = "v1"

// Version negotiates the API version from the X-API-Version header. Absent ⇒
// the default (SupportedVersion) is assumed. A header naming any other version
// is 406 Not Acceptable — the version-negotiation contract (API-01e).
func Version() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if v := r.Header.Get("X-API-Version"); v != "" && v != SupportedVersion {
				writeError(w, http.StatusNotAcceptable, "unsupported API version: "+v)
				return
			}
			w.Header().Set("X-API-Version", SupportedVersion)
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimit applies a per-tenant token bucket (API-01d noisy-neighbor
// protection). The key is the authenticated tenant (falling back to the
// subject, then the remote addr for unauthenticated/dev paths), so one
// tenant's burst cannot starve another. A non-positive rate disables it.
func RateLimit(perSec float64, burst int) func(http.Handler) http.Handler {
	if perSec <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	if burst <= 0 {
		burst = 1
	}
	lim := &tenantLimiter{perSec: perSec, burst: float64(burst), buckets: map[string]*bucket{}, now: time.Now}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !lim.allow(rateKey(r)) {
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func rateKey(r *http.Request) string {
	if p := PrincipalFromContext(r.Context()); p != nil {
		if p.Tenant != "" {
			return "t:" + p.Tenant
		}
		if p.Subject != "" {
			return "s:" + p.Subject
		}
	}
	return "a:" + r.RemoteAddr
}

type bucket struct {
	tokens float64
	last   time.Time
}

// take lazily refills the bucket by elapsed time (no background goroutine, so
// an idle gateway holds no timers), caps it at burst, and consumes one token —
// returning false when empty. Shared by the global RateLimit and the per-tenant
// Quota (MT-01e). Caller holds the owning mutex.
func (b *bucket) take(now time.Time, perSec, burst float64) bool {
	b.tokens += now.Sub(b.last).Seconds() * perSec
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type tenantLimiter struct {
	perSec  float64
	burst   float64
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

// allow consumes one token from the key's bucket, returning false when empty.
func (l *tenantLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		l.buckets[key] = &bucket{tokens: l.burst - 1, last: now}
		return true
	}
	return b.take(now, l.perSec, l.burst)
}

// Idempotency replays the prior response for a repeated Idempotency-Key
// (API-01d). It applies only to non-idempotent methods (POST) — GETs are
// already idempotent. A key seen within the window short-circuits with the
// cached status+body, so a client safely retries without re-evaluating. The
// store is bounded + TTL'd; no key ⇒ pass through.
func Idempotency(ttl time.Duration, max int) func(http.Handler) http.Handler {
	store := newIdemStore(ttl, max)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Idempotency-Key")
			if key == "" || r.Method == http.MethodGet {
				next.ServeHTTP(w, r)
				return
			}
			if cached, ok := store.get(key); ok {
				w.Header().Set("Idempotent-Replayed", "true")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(cached.status)
				_, _ = w.Write(cached.body)
				return
			}
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			// Only cache definite outcomes (not 5xx, which a retry should re-attempt).
			if rec.status < 500 {
				store.put(key, cachedResponse{status: rec.status, body: rec.buf})
			}
		})
	}
}

type cachedResponse struct {
	status int
	body   []byte
}

type idemEntry struct {
	resp    cachedResponse
	expires time.Time
}

type idemStore struct {
	ttl     time.Duration
	max     int
	mu      sync.Mutex
	entries map[string]idemEntry
	now     func() time.Time
}

func newIdemStore(ttl time.Duration, max int) *idemStore {
	if ttl <= 0 {
		ttl = time.Minute
	}
	if max <= 0 {
		max = 10_000
	}
	return &idemStore{ttl: ttl, max: max, entries: map[string]idemEntry{}, now: time.Now}
}

func (s *idemStore) get(key string) (cachedResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || s.now().After(e.expires) {
		if ok {
			delete(s.entries, key)
		}
		return cachedResponse{}, false
	}
	return e.resp, true
}

func (s *idemStore) put(key string, resp cachedResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= s.max {
		// Bound memory: drop expired first, else evict one arbitrary entry.
		now := s.now()
		for k, e := range s.entries {
			if now.After(e.expires) {
				delete(s.entries, k)
			}
		}
		if len(s.entries) >= s.max {
			for k := range s.entries {
				delete(s.entries, k)
				break
			}
		}
	}
	s.entries[key] = idemEntry{resp: resp, expires: s.now().Add(s.ttl)}
}

// recorder captures the status + body a downstream handler writes so
// Idempotency can cache and replay it.
type recorder struct {
	http.ResponseWriter
	status int
	buf    []byte
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.buf = append(r.buf, b...)
	return r.ResponseWriter.Write(b)
}

// Signing verifies an HMAC-SHA256 X-Signature over "METHOD\nPATH\nBODY"
// (API-01d request signing). Enforced only when secret is non-empty; the body
// is buffered and restored so downstream handlers still read it. A missing or
// bad signature is 401.
func Signing(secret string) func(http.Handler) http.Handler {
	if secret == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	key := []byte(secret)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sig := r.Header.Get("X-Signature")
			if sig == "" {
				writeError(w, http.StatusUnauthorized, "missing request signature")
				return
			}
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(r.Method + "\n" + r.URL.Path + "\n"))
			mac.Write(body)
			want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
			if !hmac.Equal([]byte(want), []byte(sig)) {
				writeError(w, http.StatusUnauthorized, "invalid request signature")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
