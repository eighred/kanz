package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

func TestCustodyActionsRequireFundAndForwardVerifiedActor(t *testing.T) {
	for _, tc := range []struct {
		role, configured string
		want             int
	}{{"reader", "fund", 403}, {"fund", "fund", 200}, {"fund", "", 404}} {
		t.Run(tc.role+"/"+tc.configured, func(t *testing.T) {
			be := &fakeBackend{resp: Response{Status: 200}}
			mux := authz.NewMux(authz.Grants{"reader": {authz.Read}, "fund": {authz.Fund}}, nil)
			New(be, Roles{Fund: tc.configured}).Routes(mux)
			req := httptest.NewRequest("POST", "/v1/custody/breaks/b1/actions", strings.NewReader(`{"action":"claim","request_id":"req-1","expected_revision":"1"}`))
			req = req.WithContext(middleware.WithPrincipal(req.Context(), &middleware.Principal{Subject: "verified-human", Tenant: "tenant-one", Roles: []string{tc.role}}))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status %d want %d", w.Code, tc.want)
			}
			if tc.want == 200 {
				if be.last.Principal == nil || be.last.Principal.Subject != "verified-human" || be.last.Principal.Tenant != "tenant-one" {
					t.Fatalf("lost identity: %+v", be.last)
				}
			} else if be.last.Service != "" {
				t.Fatal("refused action forwarded")
			}
		})
	}
}
