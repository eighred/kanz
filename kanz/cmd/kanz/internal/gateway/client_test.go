package gateway

import (
	"context"
	"encoding/json"
	"errors"
	tuigateway "github.com/eighred/kanz/internal/tui/gateway"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func staticToken(tok string) func() string { return func() string { return tok } }

func TestAsk(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answer":    "VaR is within limits.",
			"citations": []string{"risk-log@42"},
			"grounded":  true,
		})
	}))
	defer srv.Close()

	c := New(srv.URL, staticToken("TOK123"), "")
	res, err := c.Ask(context.Background(), "how is my risk?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if gotAuth != "Bearer TOK123" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotBody == "" || !json.Valid([]byte(gotBody)) {
		t.Fatalf("request body = %q", gotBody)
	}
	if res.Answer != "VaR is within limits." || len(res.Citations) != 1 || !res.Grounded {
		t.Fatalf("bad result: %+v", res)
	}
}

func TestMeasuresQueryParams(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"measures":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, staticToken("t"), "")
	raw, err := c.Measures(context.Background(), "PORT-1", []string{"VaR99", "Delta"}, "2026-07-10T00:00:00Z")
	if err != nil {
		t.Fatalf("Measures: %v", err)
	}
	if gotPath != "/v1/portfolios/PORT-1/measures" {
		t.Fatalf("path = %q", gotPath)
	}
	// Both measures are repeated and as_of is carried.
	if gotQuery == "" || !contains(gotQuery, "measure=VaR99") || !contains(gotQuery, "measure=Delta") || !contains(gotQuery, "as_of=") {
		t.Fatalf("query = %q", gotQuery)
	}
	if string(raw) != `{"measures":[]}` {
		t.Fatalf("raw = %s", raw)
	}
}

func TestExposureNoAsOfOmitsParam(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, staticToken("t"), "").Exposure(context.Background(), "P", ""); err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	if gotQuery != "" {
		t.Fatalf("expected no query params, got %q", gotQuery)
	}
}

func TestAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"tenant mismatch"}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, staticToken("t"), "").Exposure(context.Background(), "P", "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != http.StatusForbidden || apiErr.Message != "tenant mismatch" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
}

// A CALL WITH NO SESSION IS REFUSED HERE RATHER THAN SENT UNAUTHENTICATED.
//
// This test asserted the opposite: that an empty token merely omitted the
// Authorization header and the request went out anyway. It changed deliberately
// with #198, when this client moved onto the shared transport.
//
// The old behaviour sent a request that could only come back 401 — and a 401 is
// what an EXPIRED token looks like, so an operator who had simply never signed
// in was told their session was rejected. Refusing locally says "no token: set
// --token-file or KANZ_TOKEN", which is the thing they can act on.
//
// Nothing is reached that would otherwise have been: the request the old code
// sent was never going to be served.
func TestACallWithNoSessionIsRefusedBeforeItIsSent(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, staticToken(""), "").Exposure(context.Background(), "P", "")
	if err == nil {
		t.Fatal("a call with no session was sent — it could only ever come back 401, which " +
			"reads as an expired token to someone who has not signed in at all")
	}
	if !contains(err.Error(), "no token") {
		t.Errorf("error %q does not say a token is missing", err)
	}
	if reached {
		t.Error("the request reached the gateway despite there being no session")
	}
}

// THE SESSION IS READ FRESH, so signing in AFTER the client was built works.
//
// The REPL constructs this client at startup, before anyone has logged in — that
// is why the bearer is a function. A client that captured the token (or its
// absence) at construction would leave /login unable to repair it.
func TestASessionThatArrivesAfterConstructionIsPickedUp(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	tok := ""
	c := New(srv.URL, func() string { return tok }, "")

	if _, err := c.Exposure(context.Background(), "P", ""); err == nil {
		t.Fatal("a call before sign-in was accepted")
	}

	tok = "TOK-AFTER-LOGIN" // the /login lands
	if _, err := c.Exposure(context.Background(), "P", ""); err != nil {
		t.Fatalf("a call after sign-in still failed: %v", err)
	}
	if gotAuth != "Bearer TOK-AFTER-LOGIN" {
		t.Errorf("Authorization = %q, want the token that arrived after construction", gotAuth)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// THE DEFECT #198 WAS FILED FOR: this client sent no X-Signature at all.
//
// On any deployment setting API_GATEWAY_SIGNING_SECRET, the gateway's Signing
// middleware rejected every Copilot REPL call with a 401 — which reads as an
// expired session, sending the operator to /login, which could not fix it. The
// estate panes beside it kept working, because they signed. One shell, two
// clients, one of which silently could not reach a hardened gateway.
func TestRequestsAreSignedWhenASecretIsConfigured(t *testing.T) {
	var gotSig, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig, gotPath, gotQuery = r.Header.Get("X-Signature"), r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(srv.URL, staticToken("TOK"), "s3cret")
	if _, err := c.Exposure(context.Background(), "P", "2026-08-01T00:00:00Z"); err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	if gotSig == "" {
		t.Fatal("no X-Signature was sent — a gateway enforcing signatures would 401 this")
	}

	// THE SIGNATURE IS OVER THE PATH, NOT THE QUERY. The gateway signs
	// METHOD, r.URL.Path and the body; r.URL.Path excludes the query string. A
	// client that signed the query too would produce a signature the gateway
	// cannot reproduce — and the rejection is a 401 that reads as a bad token.
	if gotQuery == "" {
		t.Fatal("the as_of query was not sent; this test is not exercising the path/query split")
	}
	want := tuigateway.Sign([]byte("s3cret"), http.MethodGet, gotPath, nil)
	if gotSig != want {
		t.Errorf("X-Signature = %q, want %q — computed over the PATH ONLY (%q), excluding the "+
			"query (%q)", gotSig, want, gotPath, gotQuery)
	}
}

// And no signature is sent when the deployment configures none — the gateway's
// middleware is a no-op then, and sending one would be noise.
func TestNoSignatureWhenNoSecretIsConfigured(t *testing.T) {
	present := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["X-Signature"]
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, staticToken("TOK"), "").Exposure(context.Background(), "P", ""); err != nil {
		t.Fatalf("Exposure: %v", err)
	}
	if present {
		t.Error("an X-Signature was sent with no signing secret configured")
	}
}
