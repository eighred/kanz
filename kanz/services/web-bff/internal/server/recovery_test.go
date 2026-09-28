package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
)

func TestRecoveryProxyKeepsAuthorityAndErrorsServerSide(t *testing.T) {
	h := newHarness(t)
	cookie := h.login(t)
	status := 202
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Kanz-Client-IP") != "192.0.2.1" {
			t.Error("client credentials/authority forwarded")
		}
		want := ""
		if r.URL.Path == "/mailbox" {
			want = "Bearer ACCESS-TOK"
		}
		if r.Header.Get("Authorization") != want {
			t.Error("wrong bearer boundary")
		}
		w.WriteHeader(status)
		if status != 204 {
			_, _ = w.Write([]byte(`{"secret":"must-not-reach-browser"}`))
		}
	}))
	defer upstream.Close()
	h.srv.identity = identityclient.New(upstream.URL, "X-Kanz-Client-IP", time.Second)
	request := func(path, origin string, want int) {
		t.Helper()
		r := httptest.NewRequest("POST", "http://app"+path, strings.NewReader(`{}`))
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("Origin", origin)
		r.Header.Set("Authorization", "Bearer forged")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.srv.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("status=%d want=%d", w.Code, want)
		}
		if strings.Contains(w.Body.String(), "must-not-reach-browser") {
			t.Fatal("upstream secret leaked")
		}
	}
	request("/auth/recovery", "https://evil.example", 403)
	if calls != 0 {
		t.Fatal("cross-origin recovery forwarded")
	}
	request("/auth/mailbox", "http://app", 202)
	request("/auth/recovery", "http://app", 202)
	status = 401
	request("/auth/recovery/consume", "http://app", 401)
	if _, ok := h.srv.sessions.Get(cookie.Value); !ok {
		t.Fatal("failed reset destroyed session")
	}
	status = 204
	request("/auth/recovery/consume", "http://app", 204)
	if _, ok := h.srv.sessions.Get(cookie.Value); ok {
		t.Fatal("successful reset retained local session")
	}
}
