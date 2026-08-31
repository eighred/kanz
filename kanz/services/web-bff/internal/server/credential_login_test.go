package server

// CREDENTIAL LOGIN THROUGH THE BFF (#371).
//
// The property every one of these defends is the same one the package exists
// for: THE TOKEN NEVER REACHES THE BROWSER. A kanz token carries kanz-trader or
// kanz-operator, so a copy in browser-reachable JS turns any XSS anywhere in the
// SPA into a silent, full-authority theft. The browser gets an opaque cookie and
// nothing else; the BFF attaches the bearer server-side.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/clientip"
	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
	"github.com/eighred/kanz/services/web-bff/internal/session"
)

// fakeIdentity stands in for the identity service and records what it was asked.
type fakeIdentity struct {
	status   int
	body     string
	gotIP    string
	gotBody  string
	requests int
}

func (f *fakeIdentity) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests++
		f.gotIP = r.Header.Get("X-Kanz-Client-IP")
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		f.gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func bffWithIdentity(t *testing.T, identityURL string, gatewayURL string) *Server {
	t.Helper()
	ip, err := clientip.NewResolver("CF-Connecting-IP", []string{"192.0.2.1"})
	if err != nil {
		t.Fatalf("clientip: %v", err)
	}
	srv, err := New(&Readiness{}, Options{
		Identity:      identityclient.New(identityURL, "X-Kanz-Client-IP", 5*time.Second),
		ClientIP:      ip,
		Sessions:      session.NewManager(time.Hour),
		GatewayURL:    gatewayURL,
		SecureCookies: false,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func postJSON(t *testing.T, srv *Server, path, body, peer, cfIP string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = peer
	if cfIP != "" {
		r.Header.Set("CF-Connecting-IP", cfIP)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	return rec
}

// THE TOKEN IS NOWHERE IN THE RESPONSE, and the cookie is httpOnly.
func TestCredentialLoginKeepsTheTokenOffTheBrowser(t *testing.T) {
	id := &fakeIdentity{
		status: http.StatusOK,
		body:   `{"token":"the-secret-jwt","expires_at":"2099-01-01T00:00:00Z","subject":"user:alice","tenant":"acme"}`,
	}
	idSrv := id.start(t)
	srv := bffWithIdentity(t, idSrv.URL, "http://gw.invalid")

	rec := postJSON(t, srv, "/auth/login", `{"subject":"user:alice","credential":"pw"}`, "192.0.2.1:9", "198.51.100.7")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), "the-secret-jwt") {
		t.Fatalf("THE TOKEN IS IN THE RESPONSE BODY: %s\n\n"+
			"One XSS anywhere in the SPA now steals a credential carrying this user's full "+
			"authority, silently. The browser must receive an opaque cookie and nothing else.",
			rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got["subject"] != "user:alice" || got["tenant"] != "acme" {
		t.Errorf("response = %v, want subject/tenant so the SPA need not decode a token", got)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("set %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if !c.HttpOnly {
		t.Error("the session cookie is NOT httpOnly — JS can read it, which is the whole " +
			"exfiltration route this design exists to close")
	}
	if strings.Contains(c.Value, "the-secret-jwt") {
		t.Error("the cookie VALUE contains the token — it must be an opaque session id")
	}
}

// THE CALLER'S ADDRESS REACHES THE IDENTITY SERVICE, so its limiter is per-user.
func TestTheBrowsersAddressIsForwardedToIdentity(t *testing.T) {
	id := &fakeIdentity{status: http.StatusOK, body: `{"token":"t","expires_at":"2099-01-01T00:00:00Z","subject":"s","tenant":"acme"}`}
	idSrv := id.start(t)
	srv := bffWithIdentity(t, idSrv.URL, "http://gw.invalid")

	postJSON(t, srv, "/auth/login", `{"subject":"s","credential":"pw"}`, "192.0.2.1:9", "198.51.100.7")

	if id.gotIP != "198.51.100.7" {
		t.Fatalf("identity saw client ip %q, want 198.51.100.7.\n\n"+
			"Behind the tunnel every login arrives from the BFF, so without this the identity "+
			"limiter keys them all together: one user's failures throttle everybody, and an "+
			"attacker's attempts hide in the same bucket as legitimate traffic.", id.gotIP)
	}
}

// A REFUSAL IS ONE ANSWER, and a throttle is a different one.
func TestRefusalsAreIndistinguishableButThrottlingIsNot(t *testing.T) {
	for name, tc := range map[string]struct {
		identityStatus int
		wantStatus     int
	}{
		"rejected":  {http.StatusUnauthorized, http.StatusUnauthorized},
		"malformed": {http.StatusBadRequest, http.StatusUnauthorized},
		"throttled": {http.StatusTooManyRequests, http.StatusTooManyRequests},
	} {
		t.Run(name, func(t *testing.T) {
			id := &fakeIdentity{status: tc.identityStatus, body: `{}`}
			idSrv := id.start(t)
			srv := bffWithIdentity(t, idSrv.URL, "http://gw.invalid")

			rec := postJSON(t, srv, "/auth/login", `{"subject":"s","credential":"pw"}`, "192.0.2.1:9", "198.51.100.7")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if rec.Code == http.StatusUnauthorized && strings.Contains(strings.ToLower(rec.Body.String()), "password") {
				t.Error("the refusal names the credential — every refusal must read the same, or " +
					"a caller can tell an existing account from a missing one")
			}
			if len(rec.Result().Cookies()) != 0 {
				t.Error("a refused login set a cookie")
			}
		})
	}
}

// AN UNREACHABLE IDENTITY SERVICE IS A 502, NOT A 401.
//
// Reporting "those details are not valid" when the provider is down tells every
// user their password is wrong during an outage, and support spends the incident
// resetting credentials that were never broken.
func TestAnUnreachableIdentityServiceIsNotReportedAsABadPassword(t *testing.T) {
	srv := bffWithIdentity(t, "http://127.0.0.1:1", "http://gw.invalid")
	rec := postJSON(t, srv, "/auth/login", `{"subject":"s","credential":"pw"}`, "192.0.2.1:9", "198.51.100.7")
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("an unreachable identity service was reported as a rejected credential")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

// A MALFORMED BODY IS REFUSED WITHOUT CALLING THE IDENTITY SERVICE.
func TestAMalformedBodyNeverReachesIdentity(t *testing.T) {
	id := &fakeIdentity{status: http.StatusOK, body: `{}`}
	idSrv := id.start(t)
	srv := bffWithIdentity(t, idSrv.URL, "http://gw.invalid")

	rec := postJSON(t, srv, "/auth/login", `{"subject":"s","credential":"pw","extra":true}`, "192.0.2.1:9", "198.51.100.7")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown field", rec.Code)
	}
	if id.requests != 0 {
		t.Errorf("the identity service was called %d times for a malformed body — an "+
			"unauthenticated route must not spend an Argon2 verification on unparseable input",
			id.requests)
	}
}

// REDEMPTION RELAYS THE REASON; LOGIN DOES NOT (#364).
//
// A 400 from redemption is about the credential the invitee has just invented —
// too short — so it discloses nothing about the estate. Collapsed into the
// opaque 401 every other refusal gets, it reaches them as "that invitation is
// not valid", and they abandon a perfectly good single-use invitation and ask
// an operator for another one. That is the bug this asserts against.
//
// The login half is the counterweight: there, every refusal must read the same,
// because the difference between "no such account" and "wrong password" is what
// turns a sign-in form into a list of the fund's staff.
func TestRedemptionExplainsARefusedCredentialButLoginNeverDoes(t *testing.T) {
	const reason = "identity: a credential must be at least 12 characters"

	id := &fakeIdentity{status: http.StatusBadRequest, body: `{"error":"` + reason + `"}`}
	idSrv := id.start(t)
	srv := bffWithIdentity(t, idSrv.URL, "http://gw.invalid")

	rec := postJSON(t, srv, "/auth/redeem", `{"token":"t","credential":"short"}`, "192.0.2.1:9", "198.51.100.7")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("redeem status = %d, want 400 — a refused credential must not look like a "+
			"refused invitation", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "at least 12") {
		t.Fatalf("redeem body = %s, want the reason relayed.\n\n"+
			"Without it the invitee is told their invitation is invalid when their password "+
			"was merely short, and the invitation is single-use.", rec.Body.String())
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a refused redemption set a cookie")
	}

	// The SAME upstream status on the login path stays opaque.
	rec = postJSON(t, srv, "/auth/login", `{"subject":"s","credential":"short"}`, "192.0.2.1:9", "198.51.100.7")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want 401 — login refusals are one answer", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "at least 12") {
		t.Error("the login refusal relayed the identity service's reason, giving a caller a " +
			"second distinguishable response to probe with")
	}
}
