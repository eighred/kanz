package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
	"github.com/eighred/kanz/services/api-gateway/internal/gateway"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
	"github.com/eighred/kanz/services/api-gateway/internal/orders"
	"github.com/eighred/kanz/services/api-gateway/internal/proxy"
)

// THE LIMITER IS IN THE CHAIN, AND IT IS IN FRONT OF AUTHENTICATION (#835).
//
// # Why this case lives in cmd/ and not beside the middleware
//
// #835 was not a broken limiter. middleware.RateLimit was complete, tested, and
// called by NOTHING — three green unit tests certified a control the gateway did
// not run. A composition root is where that class of defect lives, because
// everything inside buildRouter is a local that no unit test in internal/ can
// see; this estate has twice shipped a startup defect with a green suite for the
// same reason (see composition_root_length_test.go in test/arch).
//
// So this drives the REAL router, and the assertion is not "a 429 appeared" —
// that could come from the per-tenant Quota, which was always wired. It is that
// THE AUTHENTICATOR STOPS BEING CALLED. In production that authenticator is a
// JWKS validation, and the request has already paid for an HMAC over its whole
// body in Signing one hop earlier; a limiter that refuses after those have run
// bounds nothing that matters.
func TestBuildRouterBoundsAnUnauthenticatedFloodBeforeItReachesAuth(t *testing.T) {
	var attempts atomic.Int64
	router := preAuthRouter(t, config.Config{
		RequiredRole: baselineRole,
		TradeRole:    traderRole,
		// The pre-auth budget IS the per-tenant budget (see buildRouter). One
		// token, and a refill slow enough that nothing returns within the test.
		RateLimitPerSec: 0.001,
		RateLimitBurst:  1,
	}, &countingAuthenticator{attempts: &attempts})

	call := func() int {
		r := httptest.NewRequest(http.MethodGet, "/v1/portfolios/PF1/exposure", nil)
		r.Header.Set("Authorization", "Bearer a-token-that-will-not-validate")
		r.RemoteAddr = "203.0.113.7:44321"
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, r)
		return rr.Code
	}

	if c := call(); c != http.StatusUnauthorized {
		t.Fatalf("first call ⇒ %d, want 401 — the caller must still be told why", c)
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("the authenticator ran %d times for one request, want 1", n)
	}

	// A NEW SOURCE PORT, deliberately. A real flood opens a connection per
	// request, and RemoteAddr is IP:PORT — keying on it verbatim would give every
	// attempt a fresh bucket, which is precisely the trap #835 named.
	r := httptest.NewRequest(http.MethodGet, "/v1/portfolios/PF1/exposure", nil)
	r.Header.Set("Authorization", "Bearer a-token-that-will-not-validate")
	r.RemoteAddr = "203.0.113.7:44322"
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, r)

	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("second call ⇒ %d, want 429 — nothing bounds the pre-auth path", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("a 429 with no Retry-After tells a client to back off without saying for how long")
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("the authenticator ran %d times, want 1 — the refusal happened AFTER token "+
			"validation, so the flood still pays for the work the limiter exists to bound", n)
	}
}

// TestBuildRouterWithNoRateLimitConfiguredStillServes pins the disabled posture
// as a deliberate one rather than an accident: a gateway with no budget
// configured passes traffic (and says so at WARN in buildRouter). Every other
// buildRouter case in this package builds a Config with no rate limit at all, so
// this is also what proves those cases are testing routing rather than silently
// being throttled.
func TestBuildRouterWithNoRateLimitConfiguredStillServes(t *testing.T) {
	var attempts atomic.Int64
	router := preAuthRouter(t, config.Config{
		RequiredRole: baselineRole,
		TradeRole:    traderRole,
	}, &countingAuthenticator{attempts: &attempts})

	for i := 0; i < 20; i++ {
		r := httptest.NewRequest(http.MethodGet, "/v1/portfolios/PF1/exposure", nil)
		r.Header.Set("Authorization", "Bearer nope")
		r.RemoteAddr = "203.0.113.7:1"
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, r)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("request %d ⇒ %d, want 401 with the limiter disabled", i, rr.Code)
		}
	}
	if n := attempts.Load(); n != 20 {
		t.Errorf("the authenticator ran %d times, want 20 — something is limiting a gateway "+
			"configured with no limit", n)
	}
}

// TestBuildRouterRefusesAHalfConfiguredProxyTrust is the config half of the same
// decision, asserted where it is enforced. A header named with nobody trusted to
// send it reads as configured and honours nothing — the pre-auth limiter would
// silently key on the proxy instead of the caller.
func TestBuildRouterRefusesAHalfConfiguredProxyTrust(t *testing.T) {
	// An unparseable CIDR must fail the BUILD, not degrade to "trust nobody":
	// buildRouter returns the error rather than serving a limiter keyed on
	// something the operator did not choose.
	_, err := buildRouterFor(t, config.Config{
		RequiredRole:       baselineRole,
		TradeRole:          traderRole,
		TrustedProxyHeader: "X-Forwarded-For",
		TrustedProxies:     []string{"not-a-cidr"},
	}, stubAuthenticator{principal: &middleware.Principal{Subject: "u", Tenant: fundingTenant}})
	if err == nil {
		t.Fatal("buildRouter accepted an unparseable trusted-proxy CIDR — the limiter would key " +
			"on the peer while the operator believes it keys on the forwarded caller")
	}
}

// countingAuthenticator refuses every token and counts how often it was asked.
// The COUNT is the assertion: in production this call is the JWKS validation the
// pre-auth limiter exists to stop a flood from paying for.
type countingAuthenticator struct{ attempts *atomic.Int64 }

func (c *countingAuthenticator) Authenticate(string) (*middleware.Principal, error) {
	c.attempts.Add(1)
	return nil, errors.New("no")
}

func preAuthRouter(t *testing.T, cfg config.Config, authn middleware.Authenticator) http.Handler {
	t.Helper()
	router, err := buildRouterFor(t, cfg, authn)
	if err != nil {
		t.Fatalf("buildRouter: %v", err)
	}
	return router
}

func buildRouterFor(t *testing.T, cfg config.Config, authn middleware.Authenticator) (http.Handler, error) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	obs, err := observability.New(context.Background(), observability.Config{
		ServiceName: "api-gateway-test", ServiceVersion: version.String(), SampleRatio: 1,
	}, slog.NewTextHandler(io.Discard, nil))
	if err != nil {
		t.Fatalf("observability: %v", err)
	}
	t.Cleanup(func() { _ = obs.Shutdown(context.Background()) })

	var ready atomic.Bool
	return buildRouter(cfg,
		gateway.New(nil, nil, nil, cfg.ApproveRole, logger),
		orders.New(nil, cfg.ApproveRole, halt.OpenGate(nil)),
		proxy.New(&recordingBackend{}, proxyRoles(cfg)),
		nil, // no control plane
		obs, &ready, logger,
		nil, // no decision recorder: these cases assert the refusal, not the audit trail
		authn,
		nil, // per-pod idempotency claims
	)
}
