package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
)

func TestInviteLifecycleProxyPinsRouteAuthorityAndReview(t *testing.T) {
	for _, action := range []string{"revoke", "reissue"} {
		t.Run(action, func(t *testing.T) {
			h := newHarness(t)
			cookie := h.login(t)
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.EscapedPath() != "/invites/source%2Fid/"+action || r.Header.Get("Authorization") != "Bearer ACCESS-TOK" || r.Header.Get("Cookie") != "" {
					t.Error("browser controlled route or authority")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != `{"revision":7}` {
					t.Error("reviewed revision changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{"invite_token":"synthetic-once"}`))
			}))
			defer upstream.Close()
			h.srv.identity = identityclient.New(upstream.URL, "", time.Second)
			for _, tc := range []struct {
				origin string
				cookie bool
				code   int
			}{{"http://app", true, 201}, {"https://evil.example", true, 403}, {"http://app", false, 401}} {
				r := httptest.NewRequest("POST", "http://app/api/identity/invites/source%2Fid/"+action, strings.NewReader(`{"revision":7}`))
				r.Header.Set("Origin", tc.origin)
				r.Header.Set("Authorization", "Bearer forged")
				if tc.cookie {
					r.AddCookie(cookie)
				}
				rr := httptest.NewRecorder()
				h.srv.ServeHTTP(rr, r)
				if rr.Code != tc.code {
					t.Fatalf("status=%d want=%d", rr.Code, tc.code)
				}
				if rr.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("one-time response cacheable")
				}
			}
			if calls != 1 {
				t.Fatal("unauthorized request reached identity")
			}
		})
	}
}
