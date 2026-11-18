package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/services/web_bff/internal/identityclient"
	"github.com/eighred/kanz/services/web_bff/internal/session"
)

func TestMFAStatusProjectsOnlyConfirmedState(t *testing.T) {
	h := newHarness(t)
	id, err := h.srv.sessions.Create(context.Background(), session.Session{Subject: "person", Tenant: "tenant", AccessToken: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mfa" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("unexpected upstream MFA request")
		}
		_, _ = fmt.Fprint(w, upstreamBody)
	}))
	defer upstream.Close()
	h.srv.identity = identityclient.New(upstream.URL, "", time.Second)
	future := time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)
	recent := func(until string) string {
		return fmt.Sprintf(`{"enabled":true,"recent":true,"verified_until":%q,"factors":[{"id":"key","name":"Security key","created_at":"2026-01-01T00:00:00Z","last_used_at":null}],"token":"private-extra"}`, until)
	}
	for _, tc := range []struct {
		name, body      string
		code            int
		enabled, recent bool
	}{
		{"unenrolled", `{"enabled":false,"recent":false,"factors":[]}`, 200, false, false},
		{"fresh", recent(future), 200, true, true},
		{"expired in transit", recent("2020-01-01T00:00:00Z"), 200, true, false},
		{"null", "null", 502, false, false},
		{"missing flags", `{"factors":[]}`, 502, false, false},
		{"null enabled", `{"enabled":null,"recent":false,"factors":[]}`, 502, false, false},
		{"wrong recent type", `{"enabled":true,"recent":"true","factors":[]}`, 502, false, false},
		{"missing factors", `{"enabled":false,"recent":false}`, 502, false, false},
		{"invalid factor", `{"enabled":true,"recent":false,"factors":[{}]}`, 502, false, false},
		{"missing cutoff", `{"enabled":true,"recent":true,"factors":[]}`, 502, false, false},
		{"malformed cutoff", recent("bad"), 502, false, false},
		{"excessive cutoff", recent(time.Now().Add(time.Hour).Format(time.RFC3339Nano)), 502, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstreamBody = tc.body
			r := httptest.NewRequest("GET", "http://app/auth/mfa", nil)
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
			w := httptest.NewRecorder()
			h.srv.ServeHTTP(w, r)
			if w.Code != tc.code || strings.Contains(w.Body.String(), "private-extra") {
				t.Fatalf("unexpected projected response: %d %s", w.Code, w.Body.String())
			}
			if tc.code == 200 {
				var status identity.MFAStatus
				if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Enabled != tc.enabled || status.Recent != tc.recent || status.Factors == nil || (status.VerifiedUntil != nil) != tc.recent {
					t.Fatalf("wrong MFA state: %s", w.Body.String())
				}
				if tc.recent && (status.VerifiedUntil.Format(time.RFC3339Nano) != future || len(status.Factors) != 1 || status.Factors[0].ID != "key") {
					t.Fatalf("lost verified MFA details: %s", w.Body.String())
				}
			}
		})
	}
}

func TestMFAProxySeparatesPublicChallengesFromBearerSessions(t *testing.T) {
	h := newHarness(t)
	id, err := h.srv.sessions.Create(context.Background(), session.Session{Subject: "person", Tenant: "tenant", AccessToken: "ACCESS-TOK"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookie, Value: id}
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
	if _, ok, _ := h.srv.sessions.Get(context.Background(), cookie.Value); !ok {
		t.Fatal("failed MFA destroyed existing session")
	}
	status = 200
	w := request("/auth/mfa/stepup/finish", "http://app", 200)
	if len(w.Result().Cookies()) == 0 {
		t.Fatal("verified MFA did not rotate cookie")
	}
	if _, ok, _ := h.srv.sessions.Get(context.Background(), cookie.Value); ok {
		t.Fatal("old BFF session retained after step-up")
	}
}
