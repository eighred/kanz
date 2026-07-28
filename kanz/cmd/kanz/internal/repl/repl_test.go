package repl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/cmd/kanz/internal/config"
	"github.com/eighred/kanz/cmd/kanz/internal/tokenstore"
	"github.com/eighred/kanz/pkg/deviceauth"
)

// fakeAuth is a stand-in Authenticator: it hands back a scripted token and
// counts how many times login ran.
type fakeAuth struct {
	tok   *deviceauth.Token
	err   error
	calls int
}

func (f *fakeAuth) Login(context.Context) (*deviceauth.Token, error) {
	f.calls++
	return f.tok, f.err
}

// harness wires a REPL to a stub gateway and captures its output.
type harness struct {
	auth *fakeAuth
	out  *strings.Builder
	repl *REPL
}

func newHarness(t *testing.T, input string, handler http.HandlerFunc) *harness {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	auth := &fakeAuth{tok: &deviceauth.Token{AccessToken: signedToken(), Expiry: time.Now().Add(time.Hour)}}
	store := tokenstore.NewAt(filepath.Join(t.TempDir(), "token.json"))
	out := &strings.Builder{}
	r := New(config.Config{GatewayURL: srv.URL}, store, auth, strings.NewReader(input), out)
	return &harness{auth: auth, out: out, repl: r}
}

func (h *harness) run(t *testing.T) string {
	t.Helper()
	if err := h.repl.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return h.out.String()
}

// signedToken is a syntactically valid (unsigned) JWT whose payload decodes to
// sub/tenant claims, for the /whoami readout.
func signedToken() string {
	// {"alg":"none"} . {"sub":"u-1","tenant":"acme"} . (empty sig)
	return "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ1LTEiLCJ0ZW5hbnQiOiJhY21lIn0."
}

func TestAskFlow(t *testing.T) {
	h := newHarness(t, "how is my risk?\n/quit\n", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ask" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("missing bearer on ask")
		}
		_, _ = w.Write([]byte(`{"answer":"Within limits.","citations":["risk-log@42"],"grounded":true}`))
	})
	out := h.run(t)
	if !strings.Contains(out, "KANZ TERMINAL") {
		t.Error("header not rendered")
	}
	if !strings.Contains(out, "Within limits.") {
		t.Error("answer not rendered")
	}
	if !strings.Contains(out, "Sources:") || !strings.Contains(out, "risk-log@42") {
		t.Error("citations not rendered")
	}
}

func TestAskUngroundedWarning(t *testing.T) {
	h := newHarness(t, "guess\n/quit\n", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answer":"maybe","grounded":false}`))
	})
	if out := h.run(t); !strings.Contains(out, "not grounded") {
		t.Errorf("missing ungrounded warning:\n%s", out)
	}
}

func TestLoginTriggeredWhenNoToken(t *testing.T) {
	h := newHarness(t, "hello\n/quit\n", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answer":"hi"}`))
	})
	// Start logged out; the first turn must run the device flow exactly once.
	h.repl.token = nil
	h.run(t)
	if h.auth.calls != 1 {
		t.Fatalf("login ran %d times, want 1", h.auth.calls)
	}
}

func TestExposureUsageOnMissingID(t *testing.T) {
	called := false
	h := newHarness(t, "/exposure\n/quit\n", func(http.ResponseWriter, *http.Request) { called = true })
	out := h.run(t)
	if called {
		t.Error("gateway should not be called without a portfolio id")
	}
	if !strings.Contains(out, "usage: /exposure") {
		t.Errorf("missing usage:\n%s", out)
	}
}

func TestStructuredCommandPrettyPrints(t *testing.T) {
	h := newHarness(t, "/measures PORT-1 VaR99\n/quit\n", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/portfolios/PORT-1/measures" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"measures":[{"name":"VaR99","value":1.5}]}`))
	})
	out := h.run(t)
	// json.Indent adds newlines + two-space nesting.
	if !strings.Contains(out, "\"measures\": [") || !strings.Contains(out, "\"value\": 1.5") {
		t.Errorf("not pretty-printed:\n%s", out)
	}
}

func TestUnknownCommand(t *testing.T) {
	h := newHarness(t, "/bogus\n/quit\n", func(http.ResponseWriter, *http.Request) {})
	if out := h.run(t); !strings.Contains(out, "unknown command") {
		t.Errorf("missing unknown-command error:\n%s", out)
	}
}

func TestHelpAndWhoami(t *testing.T) {
	h := newHarness(t, "/help\n/whoami\n/quit\n", func(http.ResponseWriter, *http.Request) {})
	h.repl.token = h.auth.tok // resume an existing session
	out := h.run(t)
	if !strings.Contains(out, "/scenario") {
		t.Error("help missing commands")
	}
	if !strings.Contains(out, "acme") || !strings.Contains(out, "u-1") {
		t.Errorf("whoami did not decode claims:\n%s", out)
	}
}

func TestLogout(t *testing.T) {
	h := newHarness(t, "/logout\n/quit\n", func(http.ResponseWriter, *http.Request) {})
	// Resume a session and persist its token so logout clears both.
	h.repl.token = h.auth.tok
	if err := h.repl.store.Save(h.auth.tok); err != nil {
		t.Fatal(err)
	}
	out := h.run(t)
	if !strings.Contains(out, "signed out") {
		t.Errorf("missing signed-out message:\n%s", out)
	}
	if tok, _ := h.repl.store.Load(); tok != nil {
		t.Error("token not deleted on logout")
	}
	if h.repl.token != nil {
		t.Error("in-memory token not cleared on logout")
	}
}

func TestSplitFirst(t *testing.T) {
	cases := []struct{ in, first, rest string }{
		{"", "", ""},
		{"one", "one", ""},
		{"  one   two  three ", "one", "two  three"},
		{"/exposure PORT-1 2026", "/exposure", "PORT-1 2026"},
	}
	for _, c := range cases {
		f, rest := splitFirst(c.in)
		if f != c.first || rest != c.rest {
			t.Errorf("splitFirst(%q) = (%q,%q), want (%q,%q)", c.in, f, rest, c.first, c.rest)
		}
	}
}
