package identityclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// THIS PACKAGE HAD NO TESTS WHILE IT LIVED IN ONE SERVICE, AND NOW IT HAS TWO
// CONSUMERS (#364). Its error mapping is not plumbing — each branch is a
// security decision about what a caller is allowed to learn — and two consumers
// means a change made for one of them silently reaches the other. So the
// decisions are pinned here rather than left to whichever caller happens to
// exercise them.

func serving(t *testing.T, status int, body string) (*Client, *[]*http.Request) {
	t.Helper()
	var got []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Clone(context.Background()))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "X-Kanz-Client-IP", 5*time.Second), &got
}

func TestLoginReturnsTheMintedToken(t *testing.T) {
	c, reqs := serving(t, http.StatusOK,
		`{"token":"jwt","expires_at":"2026-08-11T20:30:11Z","subject":"user:alice","tenant":"acme"}`)

	tok, err := c.Login(context.Background(), "user:alice", "correct horse", "203.0.113.7")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if tok.Token != "jwt" || tok.Subject != "user:alice" || tok.Tenant != "acme" {
		t.Fatalf("token = %+v", tok)
	}
	if tok.Expires.IsZero() {
		t.Error("expires_at did not decode — tokenstore.Valid would treat this session as " +
			"non-expiring and keep using a dead token")
	}
	if p := (*reqs)[0].URL.Path; p != "/login" {
		t.Errorf("path = %q, want /login", p)
	}
}

// A REFUSED CREDENTIAL IS ONE ERROR, WHATEVER THE REASON. The service collapses
// unknown-subject, wrong-credential and disabled-account into a single 401 so a
// caller cannot enumerate the platform's users; a client that unpacked them
// again would hand that capability straight back.
func TestEveryRefusalIsTheSameError(t *testing.T) {
	c, _ := serving(t, http.StatusUnauthorized, `{"error":"invalid credentials"}`)

	if _, err := c.Login(context.Background(), "user:alice", "wrong", ""); !errors.Is(err, ErrRejected) {
		t.Fatalf("Login error = %v, want ErrRejected", err)
	}
	if _, err := c.Login(context.Background(), "user:nobody", "correct horse", ""); !errors.Is(err, ErrRejected) {
		t.Fatalf("unknown-subject error = %v, want the SAME ErrRejected — anything else is a user "+
			"enumeration oracle", err)
	}
}

// A 400 IS ABOUT WHAT THE CALLER JUST TYPED, so its message IS relayed. Folding
// it into ErrRejected turns "your password is too short" into "that invitation
// is not valid", and an invitee abandons a perfectly good invitation.
func TestInvalidInputRelaysTheServiceMessage(t *testing.T) {
	c, _ := serving(t, http.StatusBadRequest, `{"error":"a credential must be at least 12 characters"}`)

	_, err := c.Redeem(context.Background(), "invite-token", "short", "")
	var invalid *ErrInvalidInput
	if !errors.As(err, &invalid) {
		t.Fatalf("Redeem error = %v, want *ErrInvalidInput", err)
	}
	if !strings.Contains(invalid.Message, "at least 12 characters") {
		t.Errorf("message = %q, want the service's own wording", invalid.Message)
	}
}

// A 400 THAT IS NOT THE EXPECTED SHAPE IS NOT PASSED THROUGH VERBATIM. An
// upstream error page is not a message for a person, and on the web it lands in
// a browser.
func TestAnUnexpectedRefusalBodyIsNotRelayed(t *testing.T) {
	c, _ := serving(t, http.StatusBadRequest, `<html><body>nginx</body></html>`)

	_, err := c.Login(context.Background(), "user:alice", "correct horse", "")
	var invalid *ErrInvalidInput
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want *ErrInvalidInput", err)
	}
	if strings.Contains(invalid.Message, "nginx") {
		t.Errorf("message = %q — an upstream body reached the caller verbatim", invalid.Message)
	}
}

// THROTTLING IS DISTINCT FROM REFUSAL, and it has to be: the caller may well
// hold a correct credential, and telling them it was wrong sends them to reset a
// password that works.
func TestThrottlingIsNotReportedAsARejection(t *testing.T) {
	c, _ := serving(t, http.StatusTooManyRequests, `{"error":"too many attempts"}`)

	_, err := c.Login(context.Background(), "user:alice", "correct horse", "")
	if !errors.Is(err, ErrThrottled) {
		t.Fatalf("error = %v, want ErrThrottled", err)
	}
	if errors.Is(err, ErrRejected) {
		t.Error("a rate-limited attempt was also reported as a rejected credential")
	}
}

// A 200 WITH NO TOKEN IS A FAILURE, not an empty session. Without this the
// caller would persist a blank bearer and every later request would 401 —
// reported as an expired session, which is the one thing it is not.
func TestAnEmptyTokenIsRefused(t *testing.T) {
	c, _ := serving(t, http.StatusOK, `{"token":"","subject":"user:alice"}`)

	if _, err := c.Login(context.Background(), "user:alice", "correct horse", ""); err == nil {
		t.Fatal("a 200 carrying no token was accepted as a session")
	}
}

// THE FORWARD HEADER IS SENT ONLY WHEN BOTH HALVES EXIST. The CLI talks to the
// identity service directly and passes no address, precisely so a client cannot
// choose which rate-limit bucket it lands in; the BFF forwards the browser's.
func TestTheClientAddressIsForwardedOnlyWhenSupplied(t *testing.T) {
	c, reqs := serving(t, http.StatusOK, `{"token":"jwt"}`)

	if _, err := c.Login(context.Background(), "user:alice", "correct horse", "203.0.113.7"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got := (*reqs)[0].Header.Get("X-Kanz-Client-IP"); got != "203.0.113.7" {
		t.Errorf("forwarded address = %q, want it set — without it the identity service keys every "+
			"forwarded login together and one user's failures throttle everybody", got)
	}

	if _, err := c.Login(context.Background(), "user:alice", "correct horse", ""); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got := (*reqs)[1].Header.Get("X-Kanz-Client-IP"); got != "" {
		t.Errorf("forwarded address = %q for a direct caller, want none — a client that can set its "+
			"own address can pick its own limiter bucket", got)
	}
}

// A CLIENT BUILT WITH NO FORWARD HEADER NEVER SENDS ONE, even when an address is
// passed. This is the CLI's configuration, and it is the belt to the braces
// above.
func TestNoForwardHeaderMeansNoneIsEverSent(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		_, _ = w.Write([]byte(`{"token":"jwt"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", 5*time.Second)
	if _, err := c.Login(context.Background(), "user:alice", "correct horse", "203.0.113.7"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	for k := range got.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-kanz-client-ip") {
			t.Errorf("header %q was sent by a client configured with no forward header", k)
		}
	}
}

// THE BASE URL'S TRAILING SLASH IS TRIMMED, so a configured
// "http://identity:8087/" does not produce "//login". Some muxes route that and
// some 404 it, and the failure reads as "the identity service is down".
func TestATrailingSlashOnTheBaseURLDoesNotDoubleThePath(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"token":"jwt"}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/", "", 5*time.Second)
	if _, err := c.Login(context.Background(), "user:alice", "correct horse", ""); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if path != "/login" {
		t.Errorf("path = %q, want /login", path)
	}
}
