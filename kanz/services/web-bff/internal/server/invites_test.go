package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/clientip"
	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
	"github.com/eighred/kanz/services/web-bff/internal/session"
)

func invitationServer(t *testing.T, identityURL string) (*Server, *http.Cookie) {
	t.Helper()
	ip, err := clientip.NewResolver("", nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(&Readiness{}, Options{
		Identity:      identityclient.New(identityURL, "", time.Second),
		ClientIP:      ip,
		Sessions:      session.NewManager(time.Hour),
		GatewayURL:    "http://gateway.invalid",
		SecureCookies: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := srv.sessions.Create(session.Session{
		AccessToken: "operator-session-token",
		Subject:     "user:operator",
		Tenant:      "acme",
		Expiry:      time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, &http.Cookie{Name: sessionCookie, Value: id}
}

func TestInvitationRouteUsesTheSessionTokenAndNeverTheBrowserAuthorization(t *testing.T) {
	var auth, cookie, body string
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		cookie = r.Header.Get("Cookie")
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"invite_token":"one-time","subject":"user:dana"}`))
	}))
	t.Cleanup(identity.Close)
	srv, sessionCookie := invitationServer(t, identity.URL)

	req := httptest.NewRequest(http.MethodPost, "/api/identity/invites", strings.NewReader(`{"subject":"user:dana"}`))
	req.Header.Set("Authorization", "Bearer browser-forgery")
	req.AddCookie(sessionCookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"invite_token":"one-time"`) {
		t.Fatalf("response = %d %s, want identity's one-time creation response", rec.Code, rec.Body.String())
	}
	if auth != "Bearer operator-session-token" {
		t.Errorf("identity authorization = %q, want the server-held session token", auth)
	}
	if cookie != "" {
		t.Errorf("identity received browser cookie %q", cookie)
	}
	if body != `{"subject":"user:dana"}` {
		t.Errorf("identity body = %q", body)
	}
}

func TestInvitationRouteRequiresABrowserSession(t *testing.T) {
	srv, _ := invitationServer(t, "http://identity.invalid")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/identity/invites", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
