package main

// PER-SUBJECT REVOCATION, AT THE LAYER WHERE IT IS ACTUALLY DECIDED (#532).
//
// The unit tests one directory down prove that middleware.Revoking refuses a
// revoked token and that revocation.Cache refuses a cold one. NEITHER OF THEM
// PROVES THIS GATEWAY USES EITHER. That gap is this repository's most expensive
// recurring defect — #535 shipped a capability no role carried, #539 shipped a
// role no route honoured, and both were green in every unit test that existed —
// so the tests here run buildAuthenticator itself and put a real, correctly
// signed token through what it returns.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/revocation"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// identityStub is the half of the identity service this gateway talks to: a
// JWKS, and the revocation feed. It mints real tokens with the same signer whose
// public half it publishes, so nothing here is a stand-in for the credential.
type identityStub struct {
	url    string
	signer *identity.Signer
	// feedUp gates the revocation endpoint, so a test can start with identity
	// unreachable and bring it up — the case that separates "held readiness"
	// from "dead pod".
	feedUp  atomic.Bool
	entries atomic.Value // []revocation.Entry
	hits    atomic.Int32
}

func newIdentityStub(t *testing.T) *identityStub {
	t.Helper()
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	st := &identityStub{}
	st.entries.Store([]revocation.Entry{})
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	st.url = srv.URL

	signer, err := identity.NewSigner(key, srv.URL, "kanz-api", time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	st.signer = signer

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": srv.URL, "jwks_uri": srv.URL + "/jwks.json",
		})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(signer.JWKS())
	})
	mux.HandleFunc("/revocations", func(w http.ResponseWriter, _ *http.Request) {
		st.hits.Add(1)
		if !st.feedUp.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(revocation.Feed{
			Kind:    revocation.FeedKind,
			AsOf:    time.Now().Unix(),
			Entries: st.entries.Load().([]revocation.Entry),
		})
	})
	return st
}

// revoke marks a subject from `at`, as identity's SetStatus does.
func (s *identityStub) revoke(subject string, at time.Time) {
	s.entries.Store([]revocation.Entry{{
		SubjectHash: revocation.HashSubject(subject), NotBefore: at.Unix(),
	}})
}

func (s *identityStub) mint(t *testing.T, subject string) string {
	t.Helper()
	tok, _, err := s.signer.Mint(&identity.User{
		Subject: subject, Tenant: "acme", Roles: []string{"kanz-user"}, Status: identity.StatusActive,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return tok
}

func testObservability(t *testing.T) *observability.Provider {
	t.Helper()
	obs, err := observability.New(context.Background(), observability.Config{
		ServiceName: "api-gateway-test", ServiceVersion: version.String(), SampleRatio: 1,
	}, slog.NewTextHandler(io.Discard, nil))
	if err != nil {
		t.Fatalf("observability: %v", err)
	}
	t.Cleanup(func() { _ = obs.Shutdown(context.Background()) })
	return obs
}

// buildWired runs the real composition root's authenticator selection.
func buildWired(t *testing.T, st *identityStub, logs *captureHandler) (middleware.Authenticator, *revocation.Cache) {
	t.Helper()
	authn, _, revs, err := buildAuthenticator(config.Config{
		OIDCIssuer:     st.url,
		OIDCAudience:   "kanz-api",
		OIDCJWKSURI:    st.url + "/jwks.json",
		RevocationsURI: st.url + "/revocations",
	}, testObservability(t), logs.logger())
	if err != nil {
		t.Fatalf("buildAuthenticator: %v", err)
	}
	return authn, revs
}

// THE WHOLE POINT, IN ONE TEST: a token this estate really minted, correctly
// signed and unexpired, is refused by the authenticator this gateway really
// builds, because identity says the account behind it was disabled.
func TestAGatewayBuiltByTheCompositionRootRefusesARevokedToken(t *testing.T) {
	st := newIdentityStub(t)
	st.feedUp.Store(true)
	logs := &captureHandler{}
	authn, revs := buildWired(t, st, logs)

	tok := st.mint(t, "trader-a")

	// Before the disable it authenticates, which is what makes the refusal below
	// evidence of the revocation and not of a broken fixture.
	if err := revs.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, err := authn.Authenticate(tok); err != nil {
		t.Fatalf("a good token was refused before any revocation: %v", err)
	}

	st.revoke("trader-a", time.Now().Add(time.Second))
	if err := revs.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	p, err := authn.Authenticate(tok)
	if p != nil || !errors.Is(err, middleware.ErrUnauthenticated) {
		t.Fatalf("the gateway admitted a disabled account's outstanding token: principal=%v err=%v. "+
			"This is #532 exactly: the disable takes effect at the NEXT login and the session in "+
			"flight keeps trading", p, err)
	}

	// AND SOMEBODY ELSE IS UNAFFECTED. A revocation control that refuses everyone
	// is an outage, not a control, and from outside the two look identical.
	if _, err := authn.Authenticate(st.mint(t, "trader-b")); err != nil {
		t.Fatalf("an unrevoked account was refused: %v", err)
	}
}

// A COLD PROCESS REFUSES RATHER THAN ADMITTING EVERYBODY. This is the cold-start
// clause of #532, asserted against the wiring rather than the cache: whatever
// the composition root does with readiness, an authenticator whose feed has
// never been fetched must not answer "not revoked".
func TestAGatewayThatHasNeverReadTheFeedRefusesRatherThanFailingOpen(t *testing.T) {
	st := newIdentityStub(t)
	st.feedUp.Store(true)
	st.revoke("trader-a", time.Now().Add(time.Second))
	logs := &captureHandler{}
	authn, _ := buildWired(t, st, logs)

	// Deliberately NOT primed.
	_, err := authn.Authenticate(st.mint(t, "trader-a"))
	if err == nil {
		t.Fatal("a gateway that has never read the revocation feed admitted a caller — every " +
			"revoked token would sail through for as long as the feed stayed unreachable")
	}
	if !errors.Is(err, middleware.ErrAuthUnavailable) {
		t.Fatalf("an unprimed gateway answered %v; want ErrAuthUnavailable (503). A 401 here tells "+
			"every signed-in user their session ended, during an identity outage", err)
	}
}

// PRIMING BLOCKS WHILE IDENTITY IS UNREACHABLE, and says so at ERROR — the level
// an operator does not filter out. Until it lands, this pod refuses everybody,
// so a pod that joined the Service here would be an outage with a green probe.
func TestPrimingIsHeldWhileTheRevocationFeedIsUnreachable(t *testing.T) {
	st := newIdentityStub(t) // feed deliberately down
	reg := prometheus.NewRegistry()
	logs := &captureHandler{}
	c, err := revocation.New(revocation.Config{
		URL: st.url + "/revocations", RefreshInterval: testRetryInterval, MaxAge: time.Minute,
	})
	if err != nil {
		t.Fatalf("revocation.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var primed atomic.Bool
	go func() {
		defer close(done)
		primed.Store(primeRevocations(ctx, c, reg, testRetryInterval, logs.logger()))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("primeRevocations did not return after its context was cancelled")
		}
	})

	waitFor(t, func() bool { return st.hits.Load() >= 2 }, "the revocation feed to be retried")
	if primed.Load() {
		t.Fatal("primeRevocations reported success while the feed was unreachable")
	}
	if got := gaugeValue(t, reg, "kanz_api_gateway_revocations_usable"); got != 0 {
		t.Errorf("kanz_api_gateway_revocations_usable = %v, want 0", got)
	}
	if got := gaugeValue(t, reg, "kanz_api_gateway_revocations_age_seconds"); got != -1 {
		t.Errorf("kanz_api_gateway_revocations_age_seconds = %v, want -1 — zero is what a perfectly "+
			"fresh feed reports, and 'never fetched' is the opposite of that", got)
	}
	if !logs.hasError("CANNOT READ IDENTITY'S REVOCATION FEED") {
		t.Errorf("no ERROR naming the unreadable feed; logged:\n%s", logs.all())
	}

	// AND IT SELF-HEALS, which is what makes holding safe rather than merely
	// strict: identity coming up later is picked up with no restart here.
	st.feedUp.Store(true)
	waitFor(t, func() bool { return primed.Load() }, "priming once the feed answered")
	if got := gaugeValue(t, reg, "kanz_api_gateway_revocations_usable"); got != 1 {
		t.Errorf("kanz_api_gateway_revocations_usable = %v, want 1", got)
	}
}

// THE DEV HS256 ARM HAS NO FEED TO WAIT FOR, so priming must not block a local
// or dev deployment forever — and must not register gauges that would sit at 0
// on a working one.
func TestPrimeRevocationsReturnsImmediatelyWithoutAFeed(t *testing.T) {
	reg := prometheus.NewRegistry()
	logs := &captureHandler{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		primeRevocations(context.Background(), nil, reg, testRetryInterval, logs.logger())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("primeRevocations blocked on the HS256 arm — there is no feed to wait for")
	}
	if _, ok := findGauge(t, reg, "kanz_api_gateway_revocations_usable"); ok {
		t.Error("the revocation gauge was registered on the HS256 arm — it would sit at 0 forever " +
			"on a deployment that has no feed to read")
	}
}
