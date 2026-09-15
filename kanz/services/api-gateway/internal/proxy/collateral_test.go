package proxy

import (
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCollateralRequiresFundAndBindsPrincipal(t *testing.T) {
	for _, role := range []string{"reader", "fund"} {
		be := &fakeBackend{resp: Response{Status: 200}}
		mux := authz.NewMux(authz.Grants{"reader": {authz.Read}, "fund": {authz.Fund}}, nil)
		New(be, Roles{Fund: "fund"}).Routes(mux)
		req := httptest.NewRequest("POST", "/v1/portfolios/PF1/collateral/actions", strings.NewReader(`{"action":"approve","expected_revision":"1"}`))
		req = req.WithContext(middleware.WithPrincipal(req.Context(), &middleware.Principal{Subject: "verified", Tenant: "tenant-A", Roles: []string{role}, Portfolios: []string{"PF1"}}))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if role == "reader" {
			if w.Code != 403 || be.last.Service != "" {
				t.Fatalf("reader forwarded: %d", w.Code)
			}
		} else if w.Code != 200 || be.last.Principal == nil || be.last.Principal.Subject != "verified" {
			t.Fatalf("identity lost: %d %+v", w.Code, be.last)
		}
	}
}
