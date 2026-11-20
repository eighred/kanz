package proxy

import (
	"net/http"
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

func TestCustodyQueueGatewayPreservesContinuationAndStatus(t *testing.T) {
	for _, tc := range []struct {
		query          string
		upstream, want int
		body           string
	}{
		{"limit=1&after=UEZ8Q3xxdWFudGl0eXzpm6o", 200, 200, `{"count":1,"next_cursor":"cTE6YWJj"}`},
		{"limit=1&after=YQ&after=Yg", 400, 400, `{"error":"Invalid custody queue cursor."}`},
		{"", 422, 422, `{"code":"custody_queue_pagination_required","limit":4096}`},
		{"limit=1&after=%ZZ", 0, 400, ""},
	} {
		calls := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.URL.Path != "/v1/custody/breaks" {
				t.Error(r.URL.Path)
			}
			if tc.upstream == 200 && (r.URL.Query().Get("after") != "UEZ8Q3xxdWFudGl0eXzpm6o" || r.URL.Query().Get("limit") != "1") {
				t.Error(r.URL.RawQuery)
			}
			if tc.upstream == 400 && strings.Join(r.URL.Query()["after"], ",") != "YQ,Yg" {
				t.Error(r.URL.RawQuery)
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.upstream)
			_, _ = w.Write([]byte(tc.body))
		}))
		be := NewMeshBackend(map[Service]string{ServiceAccounting: upstream.URL}, upstream.Client())
		m := authz.NewMux(authz.Grants{"reader": {authz.CustodyRead}}, nil)
		New(be, Roles{CustodyRead: "reader"}).Routes(m)
		r := httptest.NewRequest("GET", "/v1/custody/breaks?"+tc.query, nil)
		r = r.WithContext(middleware.WithPrincipal(r.Context(), &middleware.Principal{Subject: "reader", Tenant: "T", Roles: []string{"reader"}, Portfolios: []string{"PF"}}))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		upstream.Close()
		if w.Code != tc.want || (tc.upstream == 0 && calls != 0) || (tc.upstream != 0 && (calls != 1 || w.Body.String() != tc.body || w.Header().Get("Cache-Control") != "no-store")) {
			t.Fatalf("%q status %d calls %d body %s", tc.query, w.Code, calls, w.Body.String())
		}
	}
}
