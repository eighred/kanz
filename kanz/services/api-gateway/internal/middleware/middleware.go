package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/eighred/kanz/internal/gatewaysig"
	"github.com/eighred/kanz/pkg/bus"
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

// RateLimit IS GONE, AND ITS ABSENCE IS THE POINT (#835).
//
// It was a complete per-key token bucket — lazy refill, its own bucket set — and
// nothing in the module called it. A reviewer reading this file found a wired
// rate limiter and reasonably concluded the gateway was rate-limited; it was
// not, at the layer that mattered most. An unwired seam and a healthy one must
// not look the same.
//
// The two questions it appeared to answer are now answered by the two middleware
// that are actually in the chain: Quota bounds an authenticated TENANT
// (per-tenant budgets and overrides, MT-01e), and PreAuth bounds a SOURCE that
// cannot authenticate at all. test/arch's
// TestEveryGatewayMiddlewareIsWiredIntoTheChain fails the build if a third one
// arrives and is never called.

// rateKey is the principal the per-tenant Quota charges. Its only caller now
// runs AFTER Auth, so a principal is always present and the address arm is a
// backstop rather than a path: Auth refuses (503) without an authenticator and
// (401) without a valid token, and every authenticator this gateway accepts
// requires a subject claim. That matters because RemoteAddr is IP:PORT, which
// changes per TCP connection — as a live key it would hand every connection a
// fresh bucket. PreAuth, which does key on the address, resolves it through
// clientip and strips the port for exactly this reason.
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

// IdempotencyWith makes a repeated Idempotency-Key at-most-once and replays
// the prior response (API-01d). It applies only to non-idempotent methods — GETs are
// already idempotent — and passes through when the client sends no key.
//
// # THE KEY IS SCOPED, NOT THE HEADER AS SENT
//
// This used to key a process-global map on the raw client-supplied header, and
// the consequence was not a cache miss. Two tenants that chose the same key
// within the window SHARED an entry, and because the replay short-circuits
// before the handler, the second tenant read the first tenant's response body
// and its own request was never executed. On /v1/orders that body names an
// order id, so tenant B received `202 Accepted` and another fund's order id for
// an order B never placed — a cross-tenant leak and a silent order loss in the
// same response.
//
// It needed no attacker. `1`, `retry-1` and a client's own sequence number
// collide between tenants by accident.
//
// The scope is (tenant, subject, method, path, hash(key)). Middleware.Quota one
// line above already scopes by tenant through the same principal, which is what
// makes the omission an omission rather than a constraint. The header is hashed
// so a client cannot inject the separator or choose how much memory its key
// occupies, and the route is in the scope because a key is a promise about ONE
// operation — replaying a submit's body for a cancel would be the same defect
// wearing a different hat.
//
// # THE CLAIM IS WHAT MAKES IT AT-MOST-ONCE; THE BODY CACHE ONLY MAKES IT KIND
//
// The old store wrote its entry AFTER the handler returned, so two concurrent
// retries both missed and both executed — and concurrent retry is precisely what
// a client timeout storm is. Correctness now rests on bus.Deduper, taken BEFORE
// the handler runs: Claim reserves the key, Commit holds it for the full window
// once the outcome is definite, and Release frees it after a 5xx so a retry may
// legitimately re-attempt.
//
// bus.Deduper is the estate's one implementation of this concept — DedupWindow
// in memory (bounded, and it evicts the entry expiring SOONEST rather than an
// arbitrary live one, which the map this replaces did not) and RedisDedup across
// pods. api-gateway runs replicas: 2, so a per-pod claim is a partial answer;
// IdempotencyWith takes any Deduper so the composition root can hand it the
// Redis-backed one, exactly as risk-engine and webhook-ingest already do.
//
// A duplicate this pod cannot answer from its body cache is REFUSED with 409,
// not executed. That is the fail-loud choice CLAUDE.md asks for: the client
// learns its retry was not run, instead of the platform placing a second order.
//
// # THE CLAIM STORE IS A PARAMETER, AND THERE IS NO CONVENIENCE TWIN
//
// `Idempotency(ttl, max)` used to sit here and wrap this call with the in-memory
// window. It had no production caller — the composition root has always used
// this form so it can hand over the cross-pod bus.RedisDedup — so it was a
// second spelling of one concept, reachable only from tests, in the same file as
// the dead RateLimit (#835). A nil claims falls back to the in-memory window
// rather than disabling the guarantee silently, which is what the twin provided.
func IdempotencyWith(claims bus.Deduper, ttl time.Duration, max int) func(http.Handler) http.Handler {
	if ttl <= 0 {
		ttl = time.Minute
	}
	if max <= 0 {
		max = 10_000
	}
	if claims == nil {
		claims = bus.NewDedupWindow(ttl, max)
	}
	bodies := newReplayCache(ttl, max)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Idempotency-Key")
			if header == "" || r.Method == http.MethodGet {
				next.ServeHTTP(w, r)
				return
			}
			key := idempotencyScope(r, header)
			if cached, ok := bodies.get(key); ok {
				replay(w, cached)
				return
			}
			if !claims.Claim(key) {
				// Someone else owns this key: either in flight right now, or
				// completed on another replica whose body this pod never saw.
				// Re-read the cache first — the winner may have committed
				// between the miss above and here.
				if cached, ok := bodies.get(key); ok {
					replay(w, cached)
					return
				}
				writeError(w, http.StatusConflict, "this Idempotency-Key is already being processed, "+
					"or completed on another replica; the request was NOT executed again. Retry to "+
					"read the outcome, or use a new key for a new operation")
				return
			}
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			// Only a definite outcome holds the key. A 5xx releases it, because a
			// retry of a request that failed inside the platform must be free to
			// run — holding it would turn one transient fault into a permanently
			// unusable key.
			if rec.status < 500 {
				bodies.put(key, cachedResponse{status: rec.status, body: rec.buf})
				claims.Commit(key)
				return
			}
			claims.Release(key)
		})
	}
}

// idempotencyScope derives the cache/claim key. See IdempotencyWith for why each
// component is present.
//
// tenantLabel is reused rather than reading the principal again: it is the same
// "authenticated tenant, else anonymous" rule the quota limiter applies, and two
// answers to that question is how one of them drifts.
func idempotencyScope(r *http.Request, header string) string {
	subject := ""
	if p := PrincipalFromContext(r.Context()); p != nil {
		subject = p.Subject
	}
	sum := sha256.Sum256([]byte(header))
	// \x00 cannot appear in a header value, a path or a hex digest, so no
	// combination of components can be forged to look like another.
	return strings.Join([]string{
		tenantLabel(r), subject, r.Method, r.URL.Path, hex.EncodeToString(sum[:]),
	}, "\x00")
}

func replay(w http.ResponseWriter, cached cachedResponse) {
	w.Header().Set("Idempotent-Replayed", "true")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(cached.status)
	_, _ = w.Write(cached.body)
}

type cachedResponse struct {
	status int
	body   []byte
}

type replayEntry struct {
	resp    cachedResponse
	expires time.Time
}

// replayCache holds prior responses so a retry reads its original outcome. It is
// a CONVENIENCE, not the guarantee — bus.Deduper carries at-most-once, so an
// eviction here costs a client a replayed body (it gets a 409 instead), never a
// duplicate execution. That separation is the point: the old design put
// correctness in this map, which is why evicting an arbitrary live entry under
// load silently weakened the guarantee exactly when it mattered.
type replayCache struct {
	ttl     time.Duration
	max     int
	mu      sync.Mutex
	entries map[string]replayEntry
	now     func() time.Time
}

func newReplayCache(ttl time.Duration, max int) *replayCache {
	return &replayCache{ttl: ttl, max: max, entries: map[string]replayEntry{}, now: time.Now}
}

func (s *replayCache) get(key string) (cachedResponse, bool) {
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

func (s *replayCache) put(key string, resp cachedResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= s.max {
		now := s.now()
		for k, e := range s.entries {
			if now.After(e.expires) {
				delete(s.entries, k)
			}
		}
		// Still full: drop the entry expiring SOONEST, matching bus.DedupWindow.
		// Map iteration order is random, so the previous "delete the first one
		// range yields" evicted a live entry chosen by nothing.
		if len(s.entries) >= s.max {
			var oldest string
			var oldestAt time.Time
			for k, e := range s.entries {
				if oldest == "" || e.expires.Before(oldestAt) {
					oldest, oldestAt = k, e.expires
				}
			}
			delete(s.entries, oldest)
		}
	}
	s.entries[key] = replayEntry{resp: resp, expires: s.now().Add(s.ttl)}
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
			sig := r.Header.Get(gatewaysig.Header)
			if sig == "" {
				writeError(w, http.StatusUnauthorized, "missing request signature")
				return
			}
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			// THE SAME FUNCTION EVERY CALLER SIGNS WITH (#781). This used to
			// restate the canonicalization, which made the verifier the fourth
			// independent spelling of it — and gave services/web-bff its documented
			// reason for keeping a fifth. A caller using gatewaysig.Sign is now
			// correct BY CONSTRUCTION rather than by having read this loop
			// carefully.
			if !gatewaysig.Verify(key, r.Method, r.URL.Path, body, sig) {
				writeError(w, http.StatusUnauthorized, "invalid request signature")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
