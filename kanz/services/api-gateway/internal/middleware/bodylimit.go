package middleware

import (
	"net/http"
)

// MaxRequestBody is the largest request body the gateway will accept on /v1,
// and it is THE bound — not one of several (#887).
//
// # Why the number is 1 MiB, and where it comes from
//
// infra/deploy/api-gateway-ingress.yaml sets
// `nginx.ingress.kubernetes.io/proxy-body-size: "1m"`. A body the edge already
// refuses cannot be one the gateway is obliged to accept, so the edge's bound is
// the derivation rather than a coincidence. The two must agree or the estate has
// two different answers to one question:
// test/arch/gateway_body_limit_matches_the_edge_test.go fails the build if they
// drift.
//
// It is generous for every /v1 body that exists. The largest are a scenario
// request carrying shocks and a submitted order — both kilobytes.
//
// # Why it is declared here rather than beside a handler
//
// It used to be declared THREE times — internal/gateway/helpers.go,
// internal/orders/orders.go and internal/proxy/helpers.go each had their own
// `maxBodyBytes = 1 << 20`. Three spellings of one number is how a bound gets
// raised in one place and not the others, and AGENTS.md names the shape
// exactly: a copied helper is how a fix stops spreading. Those three now read
// this one, and the middleware package is where it belongs because this layer is
// the only one that sees a request BEFORE anything has authenticated it.
const MaxRequestBody = 1 << 20 // 1 MiB

// BodyLimit refuses a request whose body is larger than max, BEFORE any
// authentication has happened (#887).
//
// # The defect this closes
//
// Signing buffers the whole body with io.ReadAll and verifies an HMAC-SHA256
// over it, and it runs before Auth. With no bound, an unauthenticated caller who
// can reach the pod chose how much of the gateway's heap it allocated per
// request and then also made the process hash all of it. The gateway is the sole
// entry point for POST /v1/orders, so the end of that is an OOM kill on the
// process the order path depends on — reported by nothing until the pod dies.
//
// The edge's proxy-body-size covers exactly one path. The browser route
// (cloudflared → web-bff → gateway) never traverses that Ingress, and neither
// does an in-cluster peer the NetworkPolicy admits to :8080 — which
// allow-observability-scrape does for the kanz-observability namespace.
//
// # It is NOT what PreAuth does, and the two do not substitute
//
// PreAuth (#835) bounds how fast one source may FAIL to authenticate. It cannot
// help here: the first request from any source is admitted by design, its bucket
// starting full, and one request is all it takes to allocate an arbitrary body.
// That control bounds pre-auth CPU over time; this one bounds pre-auth MEMORY
// per request.
//
// # Two checks, because either alone leaves a hole
//
//  1. Content-Length, refused WITHOUT READING A BYTE. This is the one that
//     protects the heap: by the time a MaxBytesReader reports an overrun the
//     reader has already handed max bytes to the caller, so a Content-Length
//     check is the difference between allocating nothing and allocating the
//     whole ceiling.
//  2. http.MaxBytesReader on the body regardless. A chunked request carries no
//     Content-Length, and a caller may simply lie about it — neither is
//     hypothetical from an unauthenticated peer. This bounds what any downstream
//     reader can pull no matter what the header claimed.
//
// # 413, never 401
//
// "Too large" and "unsigned" are different answers and must not look the same:
// a caller told 401 for an oversized body goes and checks its signing key. The
// reader installed here surfaces as *http.MaxBytesError, and Signing maps that
// to 413 rather than letting a truncated body fail verification.
//
// # A non-positive max is the ceiling, not "unlimited"
//
// Zero is what an unset config reads as, and a limiter that answers "unbounded"
// to a misconfiguration is the defect rather than a lenient default. There is no
// posture in which this control is deliberately off, so there is no value that
// turns it off.
func BodyLimit(max int64) func(http.Handler) http.Handler {
	if max <= 0 {
		max = MaxRequestBody
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// ContentLength is -1 when unknown (chunked), which must NOT be read as
			// "smaller than max" — the MaxBytesReader below is what covers it.
			if r.ContentLength > max {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, max)
			}
			next.ServeHTTP(w, r)
		})
	}
}
