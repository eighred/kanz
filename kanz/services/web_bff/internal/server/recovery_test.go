package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/web_bff/internal/identityclient"
)

func TestMailboxReadProjectsOnlyConfirmedState(t *testing.T) {
	h := newHarness(t)
	cookie := h.login(t)
	var response any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/mailbox" || r.Header.Get("Authorization") != "Bearer ACCESS-TOK" {
			t.Error("incorrect mailbox read boundary")
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer up.Close()
	h.srv.identity = identityclient.New(up.URL, "", time.Second)
	stamp := "2026-09-01T00:00:00Z"
	for _, tc := range []struct {
		name, address, pending, delivery string
		verifiedAt                       any
		want                             int
	}{
		{"empty", "", "", "none", nil, 200},
		{"verified and pending replacement", "old@example.test", "new@example.test", "sent", stamp, 200},
		{"verified challenge consumed", "user@example.test", "", "consumed", stamp, 200},
		{"null", "", "", "", nil, 502},
		{"missing address", "", "", "none", nil, 502},
		{"missing timestamp", "", "", "none", nil, 502},
		{"no verification time", "user@example.test", "", "none", nil, 502},
		{"unknown delivery", "", "", "unknown", nil, 502},
		{"missing pending address", "", "", "sending", nil, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"verified_address": tc.address, "verified_at": tc.verifiedAt, "pending_address": tc.pending, "verification_delivery": tc.delivery, "private": "upstream-only"}
			response = body
			switch tc.name {
			case "null":
				response = nil
			case "missing address":
				delete(body, "verified_address")
			case "missing timestamp":
				delete(body, "verified_at")
			}
			r := httptest.NewRequest("GET", "http://app/auth/mailbox", nil)
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			h.srv.ServeHTTP(w, r)
			if w.Code != tc.want || strings.Contains(w.Body.String(), "upstream-only") {
				t.Fatalf("unexpected mailbox projection: %d %s", w.Code, w.Body.String())
			}
			if tc.want == 200 {
				delete(body, "private")
				var got map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || !reflect.DeepEqual(got, body) {
					t.Fatalf("unexpected mailbox status: %s", w.Body.String())
				}
			}
		})
	}
}

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
	if _, ok, _ := h.srv.sessions.Get(context.Background(), cookie.Value); !ok {
		t.Fatal("failed reset destroyed session")
	}
	status = 204
	request("/auth/recovery/consume", "http://app", 204)
	if _, ok, _ := h.srv.sessions.Get(context.Background(), cookie.Value); ok {
		t.Fatal("successful reset retained local session")
	}
}
