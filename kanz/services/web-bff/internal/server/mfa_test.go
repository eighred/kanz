package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
)

func TestMFAProxySeparatesPublicChallengesFromBearerSessions(t *testing.T) {
	h := newHarness(t)
	cookie := h.login(t)
	status := 200
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		want := "Bearer ACCESS-TOK"
		if r.URL.Path == "/mfa/login/finish" {
			want = ""
		}
		if r.Header.Get("Authorization") != want || r.Header.Get("Cookie") != "" {
			t.Error("wrong MFA authority boundary")
		}
		w.WriteHeader(status)
		if strings.HasSuffix(r.URL.Path, "/begin") {
			_, _ = fmt.Fprintf(w, `{"id":%q,"token":"must-not-leak","options":{"token":"must-not-leak","publicKey":{"challenge":%q,"rpId":"example.test","userVerification":"required"}}}`, strings.Repeat("a", 43), strings.Repeat("a", 43))
		} else {
			_, _ = w.Write([]byte(`{"token":"must-not-leak","subject":"person","tenant":"tenant","expires_at":"2099-01-01T00:00:00Z"}`))
		}
	}))
	defer upstream.Close()
	h.srv.identity = identityclient.New(upstream.URL, "", time.Second)
	request := func(path, origin string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", "http://app"+path, strings.NewReader(`{}`))
		r.Header.Set("Origin", origin)
		r.Header.Set("Authorization", "Bearer forged")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.srv.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s got %d want %d", path, w.Code, want)
		}
		if strings.Contains(w.Body.String(), "must-not-leak") {
			t.Fatal("token or opaque upstream data exposed")
		}
		return w
	}
	request("/auth/mfa/stepup/begin", "https://evil.test", 403)
	if calls != 0 {
		t.Fatal("cross-origin MFA reached identity")
	}
	request("/auth/mfa/stepup/begin", "http://app", 200)
	status = 401
	request("/auth/mfa/login/finish", "http://app", 401)
	if _, ok := h.srv.sessions.Get(cookie.Value); !ok {
		t.Fatal("failed MFA destroyed existing session")
	}
	status = 200
	w := request("/auth/mfa/stepup/finish", "http://app", 200)
	if len(w.Result().Cookies()) == 0 {
		t.Fatal("verified MFA did not rotate cookie")
	}
	if _, ok := h.srv.sessions.Get(cookie.Value); ok {
		t.Fatal("old BFF session retained after step-up")
	}
}
