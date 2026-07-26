package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testCtx gives a call the deadline that call() now requires. Production call sites all set
// one (the HTTP client carries no Timeout of its own), so a test using a bare
// context.Background() would be exercising a shape that cannot occur — see
// TestCallRefusesAContextWithNoDeadline, which is the one place that shape is the subject.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newTestSource(t *testing.T, srv *httptest.Server, signing string) *gatewaySource {
	t.Helper()
	src, err := newGatewaySource(gatewayConfig{
		BaseURL: srv.URL, Token: "test-token", SigningSecret: signing,
	})
	if err != nil {
		t.Fatalf("newGatewaySource: %v", err)
	}
	return src
}

// TestCallRefusesAContextWithNoDeadline: the HTTP client carries no Timeout of its own, so
// the caller's context is the only bound on the request. A call site that forgets a deadline
// would hang the TUI forever against an unresponsive gateway, which reads to an operator as a
// frozen program rather than an error. It must fail immediately and name itself instead.
//
// The server here hangs deliberately: if the guard regresses, this test blocks rather than
// reporting a wrong value, which is the honest signal for "the call had no bound".
func TestCallRefusesAContextWithNoDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	err := newTestSource(t, srv, "").call(context.Background(), http.MethodGet, "/v1/control/nodes", nil, nil)
	if err == nil {
		t.Fatal("call accepted a context with no deadline; with no client timeout that is an " +
			"unbounded request and the TUI would hang forever")
	}
	if !strings.Contains(err.Error(), "no context deadline") {
		t.Errorf("the error must name the actual mistake so it is fixable on sight; got: %v", err)
	}
}

// --- construction refuses to half-work ------------------------------------

func TestGatewaySourceRequiresAURL(t *testing.T) {
	_, err := newGatewaySource(gatewayConfig{Token: "t"})
	if err == nil {
		t.Fatal("a source with no gateway URL was accepted; it could only fail later, at the " +
			"first poll, as a confusing connection error")
	}
	if !strings.Contains(err.Error(), "KANZ_GATEWAY_URL") {
		t.Errorf("the error must tell an operator what to set; got: %v", err)
	}
}

func TestGatewaySourceRequiresAToken(t *testing.T) {
	_, err := newGatewaySource(gatewayConfig{BaseURL: "https://api.example.com"})
	if err == nil {
		t.Fatal("a source with no token was accepted. The gateway authenticates a PERSON; " +
			"there is no anonymous mode to fall back to.")
	}
	if !strings.Contains(err.Error(), "KANZ_TOKEN") {
		t.Errorf("the error must tell an operator what to set; got: %v", err)
	}
}

func TestGatewaySourceRejectsANonAbsoluteURL(t *testing.T) {
	_, err := newGatewaySource(gatewayConfig{BaseURL: "api.example.com", Token: "t"})
	if err == nil {
		t.Fatal("a scheme-less URL was accepted; http.NewRequest would fail later with a " +
			"message about an unsupported protocol scheme rather than about configuration")
	}
}

// --- the request the gateway actually requires -----------------------------

func TestBearerTokenIsSent(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := newTestSource(t, srv, "").listVenueKeys(testCtx(t)); err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if auth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer test-token", auth)
	}
}

// The gateway's Signing middleware computes HMAC over method \n path \n body. If
// this drifts, every write silently becomes a 401 that looks like a bad token.
func TestSignatureCoversMethodPathAndBody(t *testing.T) {
	const secret = "shared-secret"
	var gotSig, gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Signature")
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	err := newTestSource(t, srv, secret).setRegion(testCtx(t), "node-1", "asia")
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(gotMethod + "\n" + gotPath + "\n"))
	mac.Write(gotBody)
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Fatalf("signature %q does not match the gateway's algorithm %q "+
			"(HMAC-SHA256 over method\\npath\\nbody, base64 raw-url)", gotSig, want)
	}
}

// A deployment with no signing secret must not send the header — an empty-key HMAC
// is a valid-looking signature over nothing, and the gateway would reject it.
func TestNoSignatureHeaderWhenNoSecretConfigured(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["X-Signature"]
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := newTestSource(t, srv, "").listVenueKeys(testCtx(t)); err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if present {
		t.Error("X-Signature was sent with no signing secret configured")
	}
}

// --- identity rides the path -----------------------------------------------

func TestNodeIdentityIsInThePathNotTheBody(t *testing.T) {
	var path string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if err := newTestSource(t, srv, "").setRegion(testCtx(t), "node-1", "asia"); err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if path != "/v1/control/nodes/node-1/region" {
		t.Errorf("path = %q; the node's identity belongs in the path", path)
	}
	if strings.Contains(string(body), "node-1") {
		t.Errorf("the body also names the node (%s). Two spellings of one identity is how a "+
			"request relabels the node nobody was looking at.", body)
	}
}

// The credential must never appear in a URL — paths land in proxy and ingress
// access logs, request bodies over TLS do not.
func TestVenueCredentialNeverAppearsInTheURL(t *testing.T) {
	var rawURL string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawURL = r.URL.String()
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"exchangeAccountId":"12345"}`))
	}))
	defer srv.Close()

	id, err := newTestSource(t, srv, "").setVenueKeys(testCtx(t), "binance",
		venueKeys{apiKey: "SECRET-KEY", apiSecret: "SECRET-SECRET"})
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if strings.Contains(rawURL, "SECRET-KEY") || strings.Contains(rawURL, "SECRET-SECRET") {
		t.Fatalf("the credential appeared in the URL (%s) — it would be written to every "+
			"proxy access log on the path", rawURL)
	}
	if !strings.Contains(rawURL, "binance") {
		t.Errorf("the venue belongs in the path; got %s", rawURL)
	}
	if !strings.Contains(string(body), "SECRET-KEY") {
		t.Error("the credential must ride the body")
	}
	if id != "12345" {
		t.Errorf("exchange account id = %q, want 12345 (the S4b proof must reach the caller)", id)
	}
}

// --- errors an operator can act on -----------------------------------------

func TestErrorsExplainWhatToDo(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"missing signature", http.StatusUnauthorized,
			`{"error":"missing request signature"}`, "KANZ_SIGNING_SECRET"},
		{"bad token", http.StatusUnauthorized,
			`{"error":"invalid token"}`, "rejected the token"},
		{"wrong role", http.StatusForbidden,
			`{"error":"insufficient capability"}`, "API_GATEWAY_OPERATOR_ROLE"},
		{"gateway fronts no control plane", http.StatusNotFound,
			`{"error":"not found"}`, "API_GATEWAY_OPERATOR_ADDR"},
		{"capability switched off", http.StatusNotImplemented,
			`{"error":"AddNode is not configured"}`, "switched off"},
		{"operator unreachable", http.StatusBadGateway,
			`{"error":"control plane unavailable"}`, "network policy"},
		{"exchange refused the key", http.StatusPreconditionFailed,
			`{"error":"exchange rejected the credentials"}`, "exchange rejected the credentials"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := newTestSource(t, srv, "").listVenueKeys(testCtx(t))
			if err == nil {
				t.Fatalf("status %d produced no error", tc.status)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error must point at the fix.\n got: %v\nwant substring: %q", err, tc.want)
			}
		})
	}
}

// A forward-compatible gateway may add response fields this build predates. That is
// not a malformed response, and refusing it would make every TUI a deployment
// blocker for the gateway.
func TestUnknownResponseFieldsAreTolerated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"venues":[{"venue":"binance","configured":true}],"futureField":42}`))
	}))
	defer srv.Close()

	rows, err := newTestSource(t, srv, "").listVenueKeys(testCtx(t))
	if err != nil {
		t.Fatalf("an unknown response field broke decoding: %v", err)
	}
	if len(rows) != 1 || rows[0].venue != "binance" || !rows[0].configured {
		t.Fatalf("decoded rows = %+v", rows)
	}
}
