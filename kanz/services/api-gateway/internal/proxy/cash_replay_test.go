package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

func TestCashHistoryGatewayPreservesPrincipalBoundsAndCachePolicy(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		p, ok := auth.PrincipalFromHeaders(r.Header)
		if !ok || p.Subject != "operator" || p.Tenant != "tenant" || !auth.PortfolioEntitled(p.Portfolios, "PF") ||
			r.URL.Path != "/v1/portfolios/PF/cash-commits" || r.URL.Query().Get("after") != "9007199254740993" || r.URL.Query().Get("through") != "9007199254740994" || r.URL.Query().Get("limit") != "2" {
			http.Error(w, "identity or cursor lost", 400)
			return
		}
		w.Header().Add("Cache-Control", "private")
		w.Header().Add("Cache-Control", "no-store")
		w.Header().Set("X-Internal-Only", "must-not-forward")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"next_revision":"9007199254740994","records":[]}`))
	}))
	defer upstream.Close()
	handler := New(NewMeshBackend(map[Service]string{ServiceAccounting: upstream.URL}, upstream.Client()), Roles{Fund: "treasury"})
	m := authz.NewMux(authz.Grants{"treasury": {authz.Fund}, "trader": {authz.Trade}, "analyst": {authz.Read}}, nil)
	handler.Routes(m)
	for _, role := range []string{"trader", "analyst", "treasury"} {
		r := httptest.NewRequest(http.MethodGet, "/v1/portfolios/PF/cash-commits?currency=USD&after=9007199254740993&through=9007199254740994&limit=2", nil)
		r = r.WithContext(middleware.WithPrincipal(r.Context(), &middleware.Principal{Subject: "operator", Tenant: "tenant", Roles: []string{role}, Portfolios: []string{"PF"}}))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if role != "treasury" {
			if w.Code != 403 || calls.Load() != 0 {
				t.Fatalf("role %s reached financial replay: status=%d calls=%d", role, w.Code, calls.Load())
			}
			continue
		}
		if w.Code != 200 || calls.Load() != 1 || w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("X-Internal-Only") != "" || w.Body.String() != `{"next_revision":"9007199254740994","records":[]}` {
			t.Fatalf("replay response changed: status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
		}
	}
}
