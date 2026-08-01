package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testClient(t *testing.T, base, secret string) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: base, Token: StaticToken("tok"), SigningSecret: secret})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func withDeadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// THE SIGNATURE IS THE REASON THIS PACKAGE EXISTS.
//
// A request signed differently from the gateway's Signing middleware is rejected
// with a 401, and a 401 reads as an expired token — so a drifted second copy of
// Sign would send an operator to re-authenticate against a problem that had
// nothing to do with their credential. This pins the exact preimage:
// method \n path \n body, HMAC-SHA256, base64 raw-url.
// It asserts the EXACT PREIMAGE, recomputed here independently, not merely that
// every input affects the output. An earlier version of this test did the latter
// and was mutation-proven useless: replacing the preimage with method+path (no
// separators) still varies with all four inputs, so it passed while the signature
// no longer matched the gateway's. Only internal/tui/universe's own test caught
// it, which left this package depending on a consumer to guard its contract.
func TestSignIsHMACOverMethodPathBody(t *testing.T) {
	const (
		key    = "k"
		method = "POST"
		path   = "/v1/control/nodes"
	)
	body := []byte(`{"a":1}`)

	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(method + "\n" + path + "\n"))
	mac.Write(body)
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if got := Sign([]byte(key), method, path, body); got != want {
		t.Fatalf("Sign = %q, want %q — this is HMAC-SHA256 over method\\npath\\nbody, base64 "+
			"raw-url, and the gateway rejects anything else with a 401 that reads as an expired token",
			got, want)
	}

	// And every component participates, so the preimage above is not accidentally
	// ignoring one of them.
	base := Sign([]byte(key), method, path, body)
	for _, other := range []string{
		Sign([]byte("k2"), method, path, body),
		Sign([]byte(key), "GET", path, body),
		Sign([]byte(key), method, "/v1/control/other", body),
		Sign([]byte(key), method, path, []byte(`{"a":2}`)),
	} {
		if other == base {
			t.Error("Sign ignored one of key/method/path/body")
		}
	}
}

func TestDoSendsTheTokenAndSignature(t *testing.T) {
	var gotAuth, gotSig, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotSig, gotType = r.Header.Get("Authorization"), r.Header.Get("X-Signature"), r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	body := []byte(`{"a":1}`)
	if _, err := testClient(t, srv.URL, "secret").Do(withDeadline(t), "POST", "/v1/x", nil, body); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", gotAuth)
	}
	if want := Sign([]byte("secret"), "POST", "/v1/x", body); gotSig != want {
		t.Errorf("X-Signature = %q, want %q", gotSig, want)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}
}

// An empty signing secret is a VALID deployment — the gateway's middleware is a
// no-op when it holds no secret — so the header must be absent, not empty.
func TestDoOmitsTheSignatureWhenNoSecretIsConfigured(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["X-Signature"]
	}))
	defer srv.Close()

	if _, err := testClient(t, srv.URL, "").Do(withDeadline(t), "GET", "/v1/x", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if present {
		t.Error("an X-Signature header was sent with no signing secret configured")
	}
}

// X-API-Version is deliberately never sent: the gateway validates it only when
// present, and a hardcoded copy here would be a second spelling of the version,
// free to drift into a 406 that looks like an outage.
func TestDoDoesNotSendAnAPIVersionHeader(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["X-Api-Version"]
	}))
	defer srv.Close()

	if _, err := testClient(t, srv.URL, "").Do(withDeadline(t), "GET", "/v1/x", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if present {
		t.Error("X-API-Version was sent — see the comment in Do for why it must not be")
	}
}

// A CALL WITH NO DEADLINE IS REFUSED, NOT PERFORMED.
//
// The client carries no Timeout of its own, so a call site that forgets a
// deadline would hang the TUI forever on an unresponsive gateway — which looks
// like a frozen program rather than an error. It must fail immediately the first
// time it is exercised.
func TestDoRefusesACallWithNoDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	_, err := testClient(t, srv.URL, "").Do(context.Background(), "GET", "/v1/x", nil, nil)
	if err == nil {
		t.Fatal("a call with no context deadline was performed — an unresponsive gateway would hang the TUI")
	}
	if !strings.Contains(err.Error(), "no context deadline") {
		t.Errorf("error %q does not name the missing deadline", err)
	}
}

// A non-2xx is a TYPED error, so each surface can render the status in its own
// words — a 404 on /v1/control means something different from a 404 elsewhere.
func TestDoReturnsATypedStatusErrorCarryingTheDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"missing role"}`))
	}))
	defer srv.Close()

	_, err := testClient(t, srv.URL, "").Do(withDeadline(t), "GET", "/v1/x", nil, nil)
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *StatusError so callers can map the status themselves", err, err)
	}
	if se.Status != http.StatusForbidden {
		t.Errorf("Status = %d, want 403", se.Status)
	}
	if se.Detail != "missing role" {
		t.Errorf("Detail = %q, want the gateway's own error field", se.Detail)
	}
	if len(se.Body) == 0 {
		t.Error("Body is empty — a caller that wants to re-parse the payload cannot")
	}
}

// A failure body that is not the gateway's JSON shape still yields something an
// operator can read, rather than an empty detail.
func TestStatusErrorFallsBackToTheRawBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream connect error"))
	}))
	defer srv.Close()

	_, err := testClient(t, srv.URL, "").Do(withDeadline(t), "GET", "/v1/x", nil, nil)
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StatusError", err)
	}
	if se.Detail != "upstream connect error" {
		t.Errorf("Detail = %q, want the raw body when there is no JSON error field", se.Detail)
	}
}

// The constructor refuses configurations that would fail confusingly later, and
// says what to set — these messages are read by someone who does not know how the
// platform is wired.
func TestNewRefusesUnusableConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no url", Config{Token: StaticToken("t")}, "no gateway URL"},
		{"relative url", Config{BaseURL: "api.eighred.com", Token: StaticToken("t")}, "not a valid absolute URL"},
		{"no token", Config{BaseURL: "https://api.eighred.com"}, "no token"},
		{"empty token", Config{BaseURL: "https://api.eighred.com", Token: StaticToken("")}, "no token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil {
				t.Fatal("an unusable configuration was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// NON-VACUITY: a well-formed config builds, and a trailing slash on the base URL
// does not produce a double slash in the request path.
func TestNewAcceptsAGoodConfigAndTrimsTheBaseURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	}))
	defer srv.Close()

	c := testClient(t, srv.URL+"/", "")
	if _, err := c.Do(withDeadline(t), "GET", "/v1/x", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotPath != "/v1/x" {
		t.Errorf("path = %q, want /v1/x (the trailing slash on the base URL must be trimmed)", gotPath)
	}
}

// THE QUERY IS SENT BUT NOT SIGNED.
//
// The gateway signs METHOD, r.URL.Path and the body — and r.URL.Path excludes
// the query string. A client that folded "?as_of=..." into the signed path would
// compute a signature the gateway cannot reproduce, and the rejection is a 401
// that reads as a bad token. This is why Do takes query as its own argument.
func TestTheQueryIsSentButNotSigned(t *testing.T) {
	var gotSig, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig, gotPath, gotQuery = r.Header.Get("X-Signature"), r.URL.Path, r.URL.RawQuery
	}))
	defer srv.Close()

	q := url.Values{"as_of": {"2026-08-01T00:00:00Z"}, "measure": {"VaR99", "Delta"}}
	if _, err := testClient(t, srv.URL, "secret").Do(withDeadline(t), "GET", "/v1/x", q, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotQuery == "" {
		t.Fatal("the query never reached the server")
	}
	if gotPath != "/v1/x" {
		t.Errorf("path = %q, want /v1/x with the query kept out of it", gotPath)
	}
	if want := Sign([]byte("secret"), "GET", "/v1/x", nil); gotSig != want {
		t.Errorf("X-Signature = %q, want %q — signed over the PATH ONLY", gotSig, want)
	}
}

// THE TOKEN IS READ FRESH ON EVERY REQUEST, so a re-login mid-session is picked
// up without rebuilding the client. A captured string goes stale the moment
// somebody signs in again, and the symptom is a 401 that looks like an expired
// session because it is one — just not the one they think.
func TestTheTokenIsReadOnEveryRequest(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	tok := "first"
	c, err := New(Config{BaseURL: srv.URL, Token: func() string { return tok }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Do(withDeadline(t), "GET", "/v1/x", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	tok = "second" // a /login lands mid-session
	if _, err := c.Do(withDeadline(t), "GET", "/v1/x", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(seen) != 2 || seen[0] != "Bearer first" || seen[1] != "Bearer second" {
		t.Errorf("authorization headers = %v, want the second call to carry the NEW token", seen)
	}
}

// The read limit is per-caller because the surfaces differ by an order of
// magnitude: a control-plane response is small, a copilot answer with citations
// is not. A shared default that truncated one would corrupt it silently.
func TestTheReadLimitIsRespected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Token: StaticToken("t"), MaxResponseBytes: 100})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.Do(withDeadline(t), "GET", "/v1/x", nil, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(got) != 100 {
		t.Errorf("read %d bytes, want the configured limit of 100", len(got))
	}
}
