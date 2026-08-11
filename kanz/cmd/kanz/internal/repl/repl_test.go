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
	"github.com/eighred/kanz/internal/identityclient"
)

// fakeAuth is a stand-in Authenticator: it hands back a scripted token, counts
// how many times login ran, and records what it was asked to sign in with.
type fakeAuth struct {
	tok   *identityclient.Token
	err   error
	calls int
	// gotSubject and gotCredential are what the REPL passed through. They are
	// recorded because the credential must reach the authenticator UNCHANGED and
	// must not be echoed anywhere — a test that only counted calls could not tell
	// a working sign-in from one that sent an empty secret.
	gotSubject    string
	gotCredential string
}

func (f *fakeAuth) Login(_ context.Context, subject, credential string) (*identityclient.Token, error) {
	f.calls++
	f.gotSubject, f.gotCredential = subject, credential
	return f.tok, f.err
}

// fixedCredential is a stand-in CredentialSource. Real ones read from the
// environment or (eventually) a masked prompt; a test needs neither.
type fixedCredential struct {
	cred string
	err  error
}

func (f fixedCredential) Credential(context.Context, string) (string, error) {
	return f.cred, f.err
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

	auth := &fakeAuth{tok: &identityclient.Token{Token: signedToken(), Expires: time.Now().Add(time.Hour)}}
	store := tokenstore.NewAt(filepath.Join(t.TempDir(), "token.json"))
	out := &strings.Builder{}
	// THE HARNESS STARTS SIGNED IN, AND IT HAS TO NOW (#364). It used to start
	// signed OUT and let the first turn trigger a sign-in, because the device
	// flow needed no secret from the operator. Credential sign-in does, and the
	// identity service issues no refresh token, so a turn can no longer
	// authenticate on anyone's behalf — a logged-out harness would be testing the
	// refusal rather than the command. Seeding the STORE rather than the field is
	// what New actually reads, so this exercises the resume path too.
	if err := store.Save(auth.tok); err != nil {
		t.Fatalf("seed the session: %v", err)
	}
	r := New(config.Config{GatewayURL: srv.URL}, store, auth, fixedCredential{cred: "correct horse"},
		strings.NewReader(input), out)
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

// A TURN NO LONGER SIGNS IN ON THE OPERATOR'S BEHALF, AND THAT IS THE POINT
// (#364).
//
// This asserted the opposite: that the first turn ran the device flow exactly
// once. That flow needed no secret from the caller, so renewing a session
// mid-turn cost nothing. Credential sign-in needs a subject and a password, and
// the identity service issues no refresh token — so keeping this behaviour would
// mean prompting for a password in the middle of an unrelated command, which
// trains an operator to type their credential whenever the terminal asks. That
// is the reflex a phishing pane wants.
func TestATurnWithNoSessionRefusesRatherThanSigningIn(t *testing.T) {
	h := newHarness(t, "hello\n/quit\n", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answer":"hi"}`))
	})
	h.repl.token = nil // signed out, whatever the store held

	out := h.run(t)
	if h.auth.calls != 0 {
		t.Fatalf("login ran %d times — a turn must never sign in on the operator's behalf, because "+
			"that means asking for a password from an unrelated command", h.auth.calls)
	}
	if !strings.Contains(out, "/login") {
		t.Errorf("the refusal does not tell the operator what to run:\n%s", out)
	}
}

// AN EXPIRED SESSION SAYS SO, and says why it cannot renew itself. "Not signed
// in" would be a lie: the operator DID sign in, and the useful fact is that
// there is nothing to refresh from.
func TestAnExpiredSessionExplainsThatThereIsNoRefresh(t *testing.T) {
	h := newHarness(t, "hello\n/quit\n", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answer":"hi"}`))
	})
	h.repl.token = &identityclient.Token{Token: signedToken(), Expires: time.Now().Add(-time.Hour)}

	out := h.run(t)
	if h.auth.calls != 0 {
		t.Fatalf("login ran %d times on an expired session", h.auth.calls)
	}
	if !strings.Contains(out, "expired") {
		t.Errorf("output does not say the session expired:\n%s", out)
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

// devREPL builds a REPL holding a pre-minted KANZ_TOKEN, with a token store
// pointed at an empty temp dir so persistence can be observed.
func devREPL(t *testing.T, token string) (*REPL, *tokenstore.Store, *strings.Builder) {
	t.Helper()
	store := tokenstore.NewAt(filepath.Join(t.TempDir(), "token.json"))
	out := &strings.Builder{}
	r := New(
		config.Config{GatewayURL: "https://gw.invalid", DevToken: token},
		store, &fakeAuth{}, fixedCredential{cred: "unused"}, strings.NewReader(""), out,
	)
	return r, store, out
}

// A PRE-MINTED TOKEN IS ADOPTED WITHOUT SIGNING IN. Its original justification
// is gone — the identity provider now exists (#364) — but the feature is not:
// a gateway with no identity service in front of it, which is how the load
// stack and several CI steps run, has a bearer and nowhere to sign in.
func TestDevTokenIsAdoptedAsTheSession(t *testing.T) {
	r, _, _ := devREPL(t, "pre-minted")

	if r.accessToken() != "pre-minted" {
		t.Fatalf("accessToken() = %q, want the pre-minted token", r.accessToken())
	}
	if !r.devSession {
		t.Error("devSession is false — /login, /logout and /whoami would misreport the session")
	}
	// tokenstore.Valid must accept it, or every command would try to sign in.
	if !tokenstore.Valid(r.token, time.Now()) {
		t.Error("the adopted token is not Valid — a zero Expiry must mean non-expiring, since the " +
			"gateway is the authority on this bearer's lifetime")
	}
}

// IT IS NEVER PERSISTED. The store is where a real session lives across runs; a
// bearer from the environment that outlived the environment is exactly the
// stale-credential surprise this must not create.
func TestDevTokenIsNotWrittenToTheTokenStore(t *testing.T) {
	_, store, _ := devREPL(t, "pre-minted")

	tok, err := store.Load()
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if tok != nil {
		t.Errorf("the dev token was persisted (%q) — unsetting KANZ_TOKEN would no longer end the "+
			"session, and the bearer would outlive the environment that supplied it", tok.Token)
	}
}

// THE OPERATOR MUST BE ABLE TO SEE IT. The failure mode of a static bearer is
// forgetting you are on one, so the header says so every session.
func TestTheHeaderSaysTheSessionIsAKanzToken(t *testing.T) {
	r, _, out := devREPL(t, "pre-minted")
	r.header()

	if !strings.Contains(out.String(), "KANZ_TOKEN") {
		t.Errorf("header = %q, want it to name KANZ_TOKEN — a static bearer nobody can see they are "+
			"using is the only genuinely dangerous version of this", out.String())
	}
}

// /login cannot work: there is nowhere to sign in. Saying so beats an exchange
// that fails against nothing.
func TestLoginIsRefusedInADevSession(t *testing.T) {
	r, _, _ := devREPL(t, "pre-minted")

	err := r.login(context.Background(), "user:alice")
	if err == nil {
		t.Fatal("/login attempted a sign-in with no KANZ_IDENTITY_URL configured")
	}
	if !strings.Contains(err.Error(), "KANZ_TOKEN") {
		t.Errorf("error %q does not explain that the session comes from KANZ_TOKEN", err)
	}
}

// /logout would be a lie: the next start reads KANZ_TOKEN again, so the session
// returns while /whoami claimed it was gone. Point at where the credential is.
func TestLogoutExplainsWhereTheDevCredentialLives(t *testing.T) {
	r, _, out := devREPL(t, "pre-minted")
	r.logout()

	if r.token == nil {
		t.Error("/logout cleared a token that comes back on the next start — the session would " +
			"appear to end and then silently return")
	}
	if !strings.Contains(out.String(), "KANZ_TOKEN") {
		t.Errorf("output %q does not say where the credential actually lives", out.String())
	}
}

// A normal signed-in session is untouched by any of this, and the credential
// reaches the authenticator exactly as the source supplied it.
func TestASignedInSessionIsNotMarkedAsADevSession(t *testing.T) {
	store := tokenstore.NewAt(filepath.Join(t.TempDir(), "token.json"))
	auth := &fakeAuth{tok: &identityclient.Token{Token: "minted", Subject: "user:alice"}}
	r := New(
		config.Config{GatewayURL: "https://gw.invalid", IdentityURL: "https://identity.invalid"},
		store, auth, fixedCredential{cred: "correct horse"}, strings.NewReader(""), &strings.Builder{},
	)
	if r.devSession {
		t.Error("an identity-configured REPL was marked as a dev session")
	}
	if err := r.login(context.Background(), "user:alice"); err != nil {
		t.Fatalf("login on a normal session failed: %v", err)
	}
	// THE SECRET MUST ARRIVE UNCHANGED. The REPL is a courier here: it reads from
	// the CredentialSource and hands the value straight to Login. Trimming,
	// lower-casing or otherwise "helping" would silently break every credential
	// containing whatever was helped with.
	if auth.gotSubject != "user:alice" || auth.gotCredential != "correct horse" {
		t.Errorf("Login received (%q, %q), want (%q, %q)",
			auth.gotSubject, auth.gotCredential, "user:alice", "correct horse")
	}
	if tok, _ := store.Load(); tok == nil {
		t.Error("a real sign-in was not persisted — the dev-token path must not have disabled it")
	}
}

// THE CREDENTIAL IS NEVER ECHOED. A REPL prints everything else it does, so the
// one thing that must not appear in its output needs an assertion of its own —
// output is scrollback, and scrollback outlives the session.
func TestTheCredentialIsNeverWrittenToTheOutput(t *testing.T) {
	store := tokenstore.NewAt(filepath.Join(t.TempDir(), "token.json"))
	out := &strings.Builder{}
	const secret = "correct horse battery staple"
	r := New(
		config.Config{GatewayURL: "https://gw.invalid", IdentityURL: "https://identity.invalid"},
		store, &fakeAuth{tok: &identityclient.Token{Token: "minted", Subject: "user:alice"}},
		fixedCredential{cred: secret}, strings.NewReader(""), out,
	)
	if err := r.login(context.Background(), "user:alice"); err != nil {
		t.Fatalf("login: %v", err)
	}
	r.whoami()
	if strings.Contains(out.String(), secret) {
		t.Fatalf("the credential appeared in the REPL's output:\n%s", out.String())
	}
}

// A SIGN-IN WITHOUT A SUBJECT IS REFUSED, WITH USAGE. /login used to take no
// argument, so an operator's muscle memory is a bare /login — which would
// otherwise reach the identity service as an empty subject and come back as
// "credential rejected", sending them to reset a password that was never wrong.
func TestLoginWithoutASubjectExplainsItself(t *testing.T) {
	store := tokenstore.NewAt(filepath.Join(t.TempDir(), "token.json"))
	auth := &fakeAuth{tok: &identityclient.Token{Token: "minted"}}
	r := New(
		config.Config{GatewayURL: "https://gw.invalid", IdentityURL: "https://identity.invalid"},
		store, auth, fixedCredential{cred: "s"}, strings.NewReader(""), &strings.Builder{},
	)
	err := r.login(context.Background(), "")
	if err == nil {
		t.Fatal("/login with no subject was accepted")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q does not show usage", err)
	}
	if auth.calls != 0 {
		t.Errorf("the identity service was called %d times for a sign-in with no subject", auth.calls)
	}
}

// THE REPL MUST ACTUALLY PASS THE SIGNING SECRET TO ITS CLIENT (#198).
//
// The client can sign — that is tested in the gateway package. What is tested
// HERE is the wiring, and only here: removing cfg.SigningSecret from the
// gateway.New call in repl.go leaves every client-level test green while every
// real request goes out unsigned. Composition-root wiring escapes unit tests
// unless something exercises the composition root, and this defect WAS that
// exact shape — a client that could sign, never told to.
func TestTheREPLSignsItsRequestsWhenASecretIsConfigured(t *testing.T) {
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Signature")
		_, _ = w.Write([]byte(`{"answer":"ok","grounded":true}`))
	}))
	defer srv.Close()

	auth := &fakeAuth{tok: &identityclient.Token{Token: signedToken(), Expires: time.Now().Add(time.Hour)}}
	store := tokenstore.NewAt(filepath.Join(t.TempDir(), "token.json"))
	out := &strings.Builder{}
	// Signed in before the turn: a turn no longer authenticates on its own, so
	// without a session this would assert the refusal message and never reach the
	// gateway whose signature is the point.
	if err := store.Save(auth.tok); err != nil {
		t.Fatalf("seed the session: %v", err)
	}
	r := New(
		config.Config{GatewayURL: srv.URL, SigningSecret: "s3cret"},
		store, auth, fixedCredential{cred: "unused"}, strings.NewReader("how is my risk?\n/quit\n"), out,
	)
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotSig == "" {
		t.Fatal("the REPL sent an UNSIGNED request while KANZ_SIGNING_SECRET was configured — " +
			"a gateway enforcing signatures would 401 it, which reads as an expired session and " +
			"sends the operator to /login, which cannot fix it")
	}
}

// And with no secret configured, nothing is sent — the gateway's middleware is a
// no-op then, so a signature would be noise.
func TestTheREPLSendsNoSignatureWithoutASecret(t *testing.T) {
	present := true
	h := newHarness(t, "how is my risk?\n/quit\n", func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["X-Signature"]
		_, _ = w.Write([]byte(`{"answer":"ok","grounded":true}`))
	})
	h.run(t)
	if present {
		t.Error("the REPL sent an X-Signature with no signing secret configured")
	}
}
