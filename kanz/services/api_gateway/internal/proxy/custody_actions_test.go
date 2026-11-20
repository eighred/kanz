package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/services/api_gateway/internal/authz"
	"github.com/eighred/kanz/services/api_gateway/internal/middleware"
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

func TestCustodyCustomerReadRequiresScopeAndCannotWrite(t *testing.T) {
	const read = "custody-reader"
	for _, tc := range []struct {
		role         string
		scope        []string
		method, path string
		code         int
	}{
		{read, []string{"PF1"}, "GET", "/v1/custody/breaks", 200},
		{read, []string{"PF1"}, "GET", "/v1/portfolios/PF1/custody/comparison?custodian_id=C&business_date=2020-02-04", 200},
		{"fund", nil, "GET", "/v1/custody/breaks", 200},
		{read, nil, "GET", "/v1/custody/breaks", 403},
		{"baseline", []string{"PF1"}, "GET", "/v1/custody/breaks", 403},
		{read, []string{"PF1"}, "POST", "/v1/custody/breaks/b1/actions", 403},
		{read, []string{"PF1"}, "POST", "/v1/portfolios/PF1/custody/reruns", 403},
		{read, []string{"PF1"}, "POST", "/v1/portfolios/PF1/cash-movements", 403},
	} {
		be := &fakeBackend{resp: Response{Status: 200}}
		m := authz.NewMux(authz.Grants{read: {authz.CustodyRead}, "fund": {authz.Fund, authz.CustodyRead}, "baseline": {authz.Read}}, nil)
		New(be, Roles{CustodyRead: read, Fund: "fund", CustodyRerun: "rerunner"}).Routes(m)
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r = r.WithContext(middleware.WithPrincipal(r.Context(), &middleware.Principal{Subject: "operator", Tenant: "fixture", Roles: []string{tc.role}, Portfolios: tc.scope}))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if w.Code != tc.code || (tc.code != 200 && be.last.Service != "") {
			t.Fatalf("%s %s %s: %d", tc.role, tc.method, tc.path, w.Code)
		}
	}
}
