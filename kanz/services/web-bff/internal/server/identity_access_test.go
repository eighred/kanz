package server

import (
	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIdentityAccessProxyPinsAuthorityAndRejectsForgery(t *testing.T) {
	h := newHarness(t)
	cookie := h.login(t)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer ACCESS-TOK" || r.Header.Get("Cookie") != "" {
			t.Error("browser controlled identity credentials")
		}
		if r.URL.EscapedPath() != "/users/alice%2Fdesk/access" {
			t.Errorf("subject path changed: %s", r.URL.EscapedPath())
		}
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"reload before editing"}`))
	}))
	defer upstream.Close()
	h.srv.identity = identityclient.New(upstream.URL, "", time.Second)
	for _, tc := range []struct {
		origin string
		cookie bool
		status int
	}{{"http://app", true, 409}, {"https://evil.example", true, 403}, {"http://app", false, 401}} {
		r := httptest.NewRequest("PUT", "http://app/api/identity/users/alice%2Fdesk/access", strings.NewReader(`{"revision":0,"roles":["kanz-user"],"portfolios":[]}`))
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Authorization", "Bearer browser-input")
		if tc.cookie {
			r.AddCookie(cookie)
		}
		rr := httptest.NewRecorder()
		h.srv.ServeHTTP(rr, r)
		if rr.Code != tc.status {
			t.Fatalf("proxy status=%d want=%d", rr.Code, tc.status)
		}
	}
	if calls != 1 {
		t.Fatalf("forged or anonymous request reached identity: %d", calls)
	}
}
