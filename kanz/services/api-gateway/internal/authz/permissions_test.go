package authz_test

import (
	"encoding/json"
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestPermissionsAgreeWithEveryEnforcedRoute(t *testing.T) {
	grants := authz.Grants{"reader": {authz.Read}, "trader": {authz.Read, authz.Trade}, "admin": {authz.Read, authz.Operate}, "unmounted": {authz.Fund}}
	mux := authz.NewMux(grants, nil)
	mux.Handle(authz.Read, "GET /v1/permissions", mux.Permissions)
	mux.Handle(authz.Trade, "POST /v1/orders", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	mux.Handle(authz.Operate, "POST /v1/nodes", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	for _, roles := range [][]string{{"reader"}, {"trader"}, {"admin"}, {"unknown"}, {"reader", "unmounted"}, {"trader", "admin", "trader"}} {
		req := httptest.NewRequest("GET", "/v1/permissions", nil)
		req = req.WithContext(middleware.WithPrincipal(req.Context(), &middleware.Principal{Subject: "alice", Tenant: "acme", Roles: roles, Portfolios: []string{"pf-1"}}))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if !grants.Allows(roles, authz.Read) {
			if rr.Code != 403 {
				t.Fatal("ungranted discovery")
			}
			continue
		}
		var body struct {
			Capabilities []authz.Capability `json:"capabilities"`
			Routes       []authz.Route      `json:"routes"`
			Portfolios   []string           `json:"portfolios"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rr.Header().Get("Cache-Control") != "no-store" || !slices.Equal(body.Portfolios, []string{"pf-1"}) {
			t.Fatal("scope or cache contract lost")
		}
		if slices.Contains(body.Capabilities, authz.Fund) {
			t.Fatal("unmounted capability advertised")
		}
		for _, route := range mux.Routes() {
			present := slices.Contains(body.Routes, route)
			if present != grants.Allows(roles, route.Capability) {
				t.Fatalf("permission drift: %v %s", roles, route.Pattern)
			}
		}
	}
	rr := httptest.NewRecorder()
	mux.Permissions(rr, httptest.NewRequest("GET", "/v1/permissions", nil))
	if rr.Code != 403 {
		t.Fatal("anonymous permissions")
	}
}
