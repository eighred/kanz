package middleware

import (
	"net/http"

	"github.com/eighred/kanz/internal/clientip"
)

// PreAuth bounds how fast ONE SOURCE may fail to authenticate at this gateway
// (#835). It is the only limiter on this path that runs in front of the
// authentication it protects.
//
// # WHAT WAS OPEN, AND WHY THE EXISTING QUOTA COULD NOT CLOSE IT
//
// The edge chain is Version → Signing → Auth → Measure → Quota → Idempotency.
// Quota (MT-01e) is per-TENANT, so it needs a principal, so it runs AFTER Auth
// — and a caller who never authenticates never reaches it. Every unauthenticated
// request therefore ran the two most expensive operations on the path with no
// bucket to exhaust first: an HMAC-SHA256 verification over the whole body
// (Signing), and a token validation against the OIDC signing keys (Auth). A
// package-level middleware.RateLimit existed, read as if it were wired, and was
// called from nowhere in the module — so the gateway asserted a control it did
// not run.
//
// This is availability of the order path: the gateway is the sole entry point
// for POST /v1/orders.
//
// # WHAT IT KEYS ON, AND WHY THAT KEY IS NOT A HEADER
//
// The only thing available before authentication is where the request came
// from. clientip.Resolver answers that: the TCP peer, unless the peer is one of
// the operator's configured trusted proxies AND sends the configured
// forwarded-for header. Honouring a header from an unverified peer would not be
// a weaker limiter, it would be no limiter — an attacker varies the value per
// request and every attempt lands in a fresh bucket, while the metrics show a
// wide spread of well-behaved clients.
//
// CLAUDE.md licenses the reverse trust, not this one: upstreams trust the
// gateway's X-Kanz-Principal-* headers because a NetworkPolicy makes the gateway
// their only reachable caller. Nothing makes ingress-nginx the gateway's only
// reachable caller — the observability namespace reaches :8080, and web-bff
// proxies to it — so the gateway must verify the peer rather than assume it.
//
// UNCONFIGURED, THE KEY AGGREGATES: every caller arriving through the ingress
// controller shares one bucket. That is the safe direction (too strict, never
// absent), and the failure-only debit below is what keeps "too strict" from
// meaning "throttles the estate".
//
// # WHY IT DEBITS ONLY A 401, AND CHECKS WITHOUT SPENDING
//
// A token is spent only by a response of 401 — "we could not establish who you
// are", which is precisely the credential-stuffing and auth-CPU signal. The
// check on the way in does NOT spend (bucketSet.hasToken), so:
//
//   - authenticated traffic never debits, and an aggregated key therefore cannot
//     be drained by legitimate load however large it is;
//   - concurrency cannot drain it either — a spend-then-refund design would hold
//     one token per in-flight request and 429 a wide burst of perfectly valid
//     calls.
//
// 403 does NOT debit. A 403 means authentication SUCCEEDED and produced a
// principal: the caller is named, attributable, and bounded by the per-tenant
// Quota one hop later. Charging a source axis for a known principal's
// authorisation failures would let one misconfigured client behind a shared edge
// spend a budget every other tenant depends on, to bound a caller that is
// already bounded.
//
// 503 does not debit either, and that one is a deliberate fail-OPEN. Auth
// answers 503 when the JWKS cache or the revocation feed is unusable — the
// platform's fault, not the caller's. Debiting it would turn an identity-provider
// outage into a lockout that outlives the outage, punishing every client for
// being present while we could not judge them.
//
// # ONE PROCESS, NOT SHARED STATE, AND THAT IS THE RIGHT UNIT
//
// The bucket is per-replica (CLAUDE.md "best-effort, per-process"). The quantity
// being bounded is the CPU of THIS pod: each replica verifies its own HMACs and
// its own tokens, so a per-pod budget is the honest unit and a shared one would
// have to be divided by a replica count nothing here knows. A shared backend
// would also put a network round-trip in front of the cheapest possible
// rejection — spending the very resource the limiter exists to protect — and its
// outage would become the outage. Nothing is accumulated that a restart must
// keep: an empty bucket is a penalty, not a fact.
//
// # THE EDGE STILL OWNS THE FIRST BOUND
//
// infra/deploy/api-gateway-ingress.yaml sets nginx limit-rps: 100 per source IP,
// and that is the layer an internet flood hits first — it refuses before a
// request reaches a pod at all. This exists because that bound covers exactly one
// path: it does not cover the browser path (cloudflared → web-bff → gateway,
// which never traverses that ingress) nor any in-cluster peer the NetworkPolicy
// admits to :8080. A control the gateway cannot state in its own process is a
// control that leaves with a manifest edit.
func PreAuth(perSec float64, burst int, ip *clientip.Resolver, obs QuotaObserver) func(http.Handler) http.Handler {
	// A non-positive rate disables it, matching Quota's own convention. The
	// composition root says so out loud at startup rather than leaving
	// "unconfigured" and "enforcing" looking the same in a log.
	if perSec <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	b := float64(burst)
	if b < 1 {
		b = 1
	}
	failures := newBucketSet()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := preAuthKey(ip, r)
			if !failures.hasToken(key, perSec, b) {
				if obs != nil {
					obs.RateLimited(anonymousTenant)
				}
				w.Header().Set("Retry-After", "1")
				writeError(w, http.StatusTooManyRequests,
					"too many failed authentication attempts from this source")
				return
			}
			sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
			next.ServeHTTP(sw, r)
			if sw.code == http.StatusUnauthorized {
				failures.debit(key, perSec, b)
			}
		})
	}
}

// preAuthKey is the source a failure is charged to.
//
// PREFIXED, and not with any prefix rateKey uses. This bucket set is its own
// (failures, not requests) so a collision is impossible today, but the prefix is
// what keeps that true if the two ever share a store — and "a:" already means
// "remote addr" one function over.
func preAuthKey(ip *clientip.Resolver, r *http.Request) string {
	return "pa:" + ip.Resolve(r)
}
