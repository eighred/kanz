package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
)

func TestExactProposalGatewayPreservesWireAndTenant(t *testing.T) {
	const body = `{"nav":"9007199254740993","prices":{"A":"0.000000000001"}}`
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		p, ok := auth.PrincipalFromHeaders(r.Header)
		b, err := io.ReadAll(r.Body)
		if !ok || p.Tenant != "t1" || p.Subject != "u1" || r.URL.Path != "/v2/propose" || string(b) != body || err != nil {
			t.Errorf("changed contract: %s %s %+v %v", r.URL.Path, b, p, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ReadOnly":true}`))
	}))
	defer upstream.Close()
	backend := NewMeshBackend(map[Service]string{ServiceOptimization: upstream.URL}, upstream.Client())
	mux := testMux()
	New(backend, Roles{}).Routes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, authed(httptest.NewRequest(http.MethodPost, "/v2/model-portfolios/propose", strings.NewReader(body)), "u1", "t1"))
	if rr.Code != 200 || !called || rr.Body.String() != `{"ReadOnly":true}` {
		t.Fatalf("%d %s called=%v", rr.Code, rr.Body, called)
	}
}

func TestLegacyModelPortfolioRoutesKeepUpstreamVersion(t *testing.T) {
	for _, leaf := range []string{"propose", "orders"} {
		be := &fakeBackend{resp: Response{Status: 410}}
		mux := authz.NewMux(authz.Grants{"analyst": {authz.Read, authz.Trade}}, nil)
		New(be, Roles{}).Routes(mux)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, authed(httptest.NewRequest(http.MethodPost, "/v1/model-portfolios/"+leaf, strings.NewReader(`{}`)), "u1", "t1"))
		if rr.Code != 410 || be.last.Path != "/v1/"+leaf {
			t.Fatalf("%s routed as %s, status=%d", leaf, be.last.Path, rr.Code)
		}
	}
}
