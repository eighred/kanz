package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/pkg/auth"
)

// READY MUST MEAN "THIS GATEWAY CAN AUTHENTICATE SOMEBODY" (#457).
//
// It meant "the port is open". ready.Store(true) sat unconditionally at the top
// of the serve goroutine, and NewOIDCAuthenticator does no network I/O — it
// checks that the issuer string is non-empty and returns. So a gateway pointed at
// an issuer that does not exist bound its port, logged "OIDC authentication
// enabled" naming that issuer, and answered /healthz and /readyz with ok while
// rejecting every request that reached it.
//
// The estate shipped exactly that. infra/deploy/api-gateway-deploy.yaml names
// https://login.eighred.com, the SSO the owner established does not exist (#99,
// which is why #364 built the platform's own). Three green signals over a total
// authentication outage, and the natural diagnosis from those signals is a bad
// token or a broken ingress — never "the issuer was never there".
//
// THESE TESTS PIN THE COMPOSITION ROOT, which is the layer that had no test at
// all: this behaviour lives entirely in wiring, and wiring is what escapes every
// unit test in the package.

// testRetryInterval is what these tests wait between probe attempts: the
// behaviour under test is WHEN readiness flips, not how long the gateway sleeps
// between attempts.
const testRetryInterval = 20 * time.Millisecond

// startProbe runs primeIssuer and guarantees the goroutine is FINISHED before the
// test returns.
//
// BOTH HALVES OF THIS ARE THE FIX FOR A RACE THAT REACHED MAIN (#457).
//
// The interval used to be a package-level var that each test overwrote and
// restored in t.Cleanup. Two things were wrong, and `-race` in CI found them
// within one merge of each other:
//
//  1. A TEST-ONLY MUTABLE GLOBAL IS STILL A GLOBAL. The cleanup's write raced a
//     probe goroutine's read of the same variable. It is now a parameter, so
//     there is no shared state to race on.
//  2. THE GOROUTINE OUTLIVED ITS TEST. Cancelling the context is not waiting for
//     it; a probe left running past t.Cleanup can touch anything the test owned.
//     Waiting is what makes the leak impossible rather than merely unlikely.
//
// Worth stating plainly: none of this was visible locally. `-race` needs cgo and
// does not run on the usual development box, so a concurrency claim here is
// unproven until CI says otherwise — which is exactly what AGENTS.md warns and
// what this test disregarded.
func startProbe(t *testing.T, a *auth.OIDCAuthenticator, reg prometheus.Registerer, issuer string, ready *atomic.Bool, logs *captureHandler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		primeIssuer(ctx, a, reg, issuer, testRetryInterval, ready, logs.logger())
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("primeIssuer did not return after its context was cancelled — a probe that " +
				"outlives the gateway's shutdown keeps a pod alive that is trying to leave")
		}
	})
}

// jwksServer serves a real discovery document and JWKS, and can be switched on
// late — which is the case that separates "held readiness" from "dead pod".
type jwksServer struct {
	url  string
	up   atomic.Bool
	hits atomic.Int32
}

func newJWKSServer(t *testing.T) *jwksServer {
	t.Helper()
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	js := &jwksServer{}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	js.url = srv.URL

	signer, err := identity.NewSigner(key, srv.URL, "kanz-api", time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		js.hits.Add(1)
		if !js.up.Load() {
			// The shape of a provider that is not there yet.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   srv.URL,
			"jwks_uri": srv.URL + "/jwks.json",
		})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(signer.JWKS())
	})
	return js
}

func authenticatorFor(t *testing.T, issuer string) *auth.OIDCAuthenticator {
	t.Helper()
	a, err := auth.NewOIDCAuthenticator(auth.OIDCConfig{Issuer: issuer, Audience: "kanz-api"})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	return a
}

// A GATEWAY THAT CANNOT REACH ITS ISSUER IS NOT READY, and says so where an
// operator can see it. This is the shipped configuration's exact case.
func TestReadinessIsHeldWhileTheIssuerIsUnreachable(t *testing.T) {
	js := newJWKSServer(t) // deliberately left down
	reg := prometheus.NewRegistry()
	logs := &captureHandler{}
	var ready atomic.Bool
	startProbe(t, authenticatorFor(t, js.url), reg, js.url, &ready, logs)

	// It must never become ready — waiting past the first retry proves the loop
	// is retrying rather than having flipped ready on its way out.
	waitFor(t, func() bool { return js.hits.Load() >= 2 }, "the issuer to be retried")
	if ready.Load() {
		t.Fatal("the gateway declared itself READY while it could not verify a single token — " +
			"Kubernetes would route traffic to a pod that 401s everything")
	}
	if got := gaugeValue(t, reg, "kanz_api_gateway_oidc_issuer_reachable"); got != 0 {
		t.Errorf("kanz_api_gateway_oidc_issuer_reachable = %v, want 0", got)
	}
	// AND IT SAYS SO AT ERROR. An operator who filters WARN would see nothing but
	// a healthy pod rejecting every request, which is how this survived.
	if !logs.hasError("CANNOT REACH THE OIDC ISSUER") {
		t.Errorf("no ERROR naming the unreachable issuer; logged:\n%s", logs.all())
	}
}

// AND IT SELF-HEALS. The retry is what makes holding readiness safe rather than
// merely strict: an issuer that comes up later — a co-deployed IdP, a restarted
// provider — is picked up with no restart of this gateway.
func TestReadinessArrivesWhenTheIssuerDoes(t *testing.T) {
	js := newJWKSServer(t)
	reg := prometheus.NewRegistry()
	logs := &captureHandler{}
	var ready atomic.Bool
	startProbe(t, authenticatorFor(t, js.url), reg, js.url, &ready, logs)

	waitFor(t, func() bool { return js.hits.Load() >= 1 }, "the first probe")
	if ready.Load() {
		t.Fatal("ready before the issuer answered")
	}

	js.up.Store(true)
	waitFor(t, func() bool { return ready.Load() }, "readiness after the issuer came up")

	if got := gaugeValue(t, reg, "kanz_api_gateway_oidc_issuer_reachable"); got != 1 {
		t.Errorf("kanz_api_gateway_oidc_issuer_reachable = %v, want 1", got)
	}
	if !logs.hasInfo("issuer reached and keys held") {
		t.Errorf("no success line once keys were in hand; logged:\n%s", logs.all())
	}
}

// THE DEV HS256 ARM HAS NOTHING TO REACH, so it must not be held. A nil
// authenticator means the gateway validates with a secret already in hand;
// blocking readiness on a provider that does not exist for that arm would make
// every local and dev deployment permanently unready.
func TestPrimeIssuerReturnsImmediatelyWithoutOIDC(t *testing.T) {
	reg := prometheus.NewRegistry()
	logs := &captureHandler{}
	done := make(chan struct{})
	var ready atomic.Bool
	go func() {
		defer close(done)
		primeIssuer(context.Background(), nil, reg, "", testRetryInterval, &ready, logs.logger())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("primeIssuer blocked on the HS256 arm — there is no provider to wait for")
	}
	// It must not register the gauge either: a gateway with no OIDC issuer
	// exporting "issuer reachable 0" would alert forever on a working deployment.
	if _, ok := findGauge(t, reg, "kanz_api_gateway_oidc_issuer_reachable"); ok {
		t.Error("the OIDC posture gauge was registered on the HS256 arm — it would sit at 0 " +
			"forever on a deployment that has no issuer to reach")
	}
}

// A PROVIDER ANSWERING FOR A DIFFERENT ISSUER MUST NOT SATISFY THE PROBE. This is
// the case a reachability ping waves through and the reason primeIssuer runs the
// real discovery: keys fetched from an issuer other than the configured one would
// validate tokens minted by somebody else.
func TestReadinessIsHeldWhenDiscoveryNamesAnotherIssuer(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	var probes atomic.Int32
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   "https://someone-elses-idp.example",
			"jwks_uri": srv.URL + "/jwks.json",
		})
	})

	reg := prometheus.NewRegistry()
	logs := &captureHandler{}
	var ready atomic.Bool
	startProbe(t, authenticatorFor(t, srv.URL), reg, srv.URL, &ready, logs)

	waitFor(t, func() bool { return probes.Load() >= 1 }, "the discovery probe")
	// Give the goroutine room to flip ready if it were going to.
	time.Sleep(200 * time.Millisecond)
	if ready.Load() {
		t.Fatal("a provider answering for ANOTHER issuer satisfied the readiness probe — this " +
			"gateway would validate tokens against keys belonging to somebody else")
	}
}

// waitFor polls until cond holds, failing with what it was waiting for rather
// than a bare timeout.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	v, ok := findGauge(t, reg, name)
	if !ok {
		t.Fatalf("gauge %q is not registered — an absent posture reads as 'no data', not as "+
			"'this gateway cannot authenticate anybody'", name)
	}
	return v
}

func findGauge(t *testing.T, reg *prometheus.Registry, name string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if m.GetGauge() != nil {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// captureHandler records what was logged, at what level.
//
// LEVEL IS PART OF THE ASSERTION, not decoration. The whole complaint in #457 is
// that a total authentication outage was invisible; a line logged below the level
// an operator filters on is invisible in exactly the way that matters, so a test
// that matched the text alone would pass on a regression to WARN.
type captureHandler struct {
	mu   sync.Mutex
	recs []logRecord
}

type logRecord struct {
	level string
	msg   string
}

func (c *captureHandler) logger() *slog.Logger { return slog.New(c) }

func (c *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, logRecord{level: r.Level.String(), msg: r.Message})
	return nil
}

func (c *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(string) slog.Handler      { return c }

func (c *captureHandler) records() []logRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]logRecord(nil), c.recs...)
}

func (c *captureHandler) all() string {
	var b strings.Builder
	for _, r := range c.records() {
		b.WriteString(r.level + " " + r.msg + "\n")
	}
	return b.String()
}

func (c *captureHandler) hasError(substr string) bool { return c.hasAt("ERROR", substr) }
func (c *captureHandler) hasInfo(substr string) bool  { return c.hasAt("INFO", substr) }
func (c *captureHandler) hasWarn(substr string) bool  { return c.hasAt("WARN", substr) }

func (c *captureHandler) hasAt(level, substr string) bool {
	for _, r := range c.records() {
		if r.level == level && strings.Contains(r.msg, substr) {
			return true
		}
	}
	return false
}
