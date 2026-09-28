package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
)

func TestRotationProxyPinsBearerSourceAndDeletesSessionOnlyOnSuccess(t *testing.T) {
	h := newHarness(t)
	cookie := h.login(t)
	status, calls := 400, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/credential" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer ACCESS-TOK" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Kanz-Client-IP") != "192.0.2.1" {
			t.Error("rotation authority or source changed")
		}
		w.WriteHeader(status)
		if status != 204 {
			_, _ = w.Write([]byte(`{"error":"secret-upstream-credential-material"}`))
		}
	}))
	defer upstream.Close()
	h.srv.identity = identityclient.New(upstream.URL, "X-Kanz-Client-IP", time.Second)
	request := func(origin string, want int) {
		t.Helper()
		r := httptest.NewRequest("POST", "http://app/auth/credential", strings.NewReader(`{"current_credential":"synthetic-current","new_credential":"synthetic-replacement"}`))
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("Origin", origin)
		r.Header.Set("Authorization", "Bearer forged")
		r.Header.Set("X-Kanz-Client-IP", "forged")
		r.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.srv.ServeHTTP(rr, r)
		if rr.Code != want {
			t.Fatalf("got=%d want=%d", rr.Code, want)
		}
		if strings.Contains(rr.Body.String(), "secret-upstream") {
			t.Fatal("upstream leaked")
		}
	}
	request("https://evil.example", 403)
	if calls != 0 {
		t.Fatal("cross-origin rotation reached identity")
	}
	request("http://app", 400)
	if _, ok := h.srv.sessions.Get(cookie.Value); !ok {
		t.Fatal("failed rotation destroyed session")
	}
	status = 204
	request("http://app", 204)
	if _, ok := h.srv.sessions.Get(cookie.Value); ok {
		t.Fatal("rotation left local session active")
	}
	request("http://app", 401)
	if calls != 2 {
		t.Fatal("unauthenticated rotation reached identity")
	}
}
