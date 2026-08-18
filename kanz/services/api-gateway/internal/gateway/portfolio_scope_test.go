package gateway_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/gateway"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// THE RISK READ ROUTES ENFORCE THE TENANT BOUNDARY AND NOT THE PORTFOLIO ONE
// (#99).
//
// #222 closed the cross-TENANT hole on exposure, measures and scenario by
// reading owner_tenant off the reply. It closed one dimension of two.
//
// Within a tenant the platform restricts a caller by the `portfolios` claim, and
// two routes already honour it:
//
//	GET /v1/portfolios              -> filtered by auth.PortfolioInScope
//	GET /v1/portfolios/{id}/orders  -> refused by auth.PortfolioInScope
//
// These three do not. So a caller whose token restricts them to PF1 cannot SEE
// PF2 in the list and cannot read its ORDERS — and can read its VaR, its
// exposure and run what-if scenarios on it by naming the id directly. The
// restriction is real, deliberate, and enforced everywhere except the three
// routes that disclose the most.

// serveScoped is serve() with a principal RESTRICTED to the named portfolios —
// the state a token carries once the identity provider sets portfolios on the
// invite (#386).
func serveScoped(t *testing.T, fc *fakeClient, portfolios ...string) *httptest.Server {
	t.Helper()
	mux := authz.NewMux(authz.Grants{"analyst": {authz.Read}}, nil)
	gateway.New(fc, fc, fc, "", nil).Routes(mux)
	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := &middleware.Principal{
			Subject: "u1", Tenant: testTenant, Roles: []string{"analyst"},
			Portfolios: portfolios,
		}
		mux.ServeHTTP(w, r.WithContext(middleware.WithPrincipal(r.Context(), p)))
	})
	ts := httptest.NewServer(authed)
	t.Cleanup(ts.Close)
	return ts
}

// A PORTFOLIO THE CALLER'S CLAIM DOES NOT NAME IS NOT THEIRS TO READ, even
// inside their own tenant.
func TestPortfolioRoutesRefuseAPortfolioOutsideTheClaim(t *testing.T) {
	const secret = "SECRET-POSITION"
	// Same tenant as the caller — so the #222 owner_tenant gate PASSES and only
	// the portfolio restriction can refuse this.
	fc := &fakeClient{
		exposureResp: &querypb.ExposureResponse{
			PortfolioId: "PF2", OwnerTenant: testTenant,
			Set: &domainpb.ExposureSet{PortfolioId: secret},
		},
		measuresResp: &querypb.MeasuresResponse{PortfolioId: secret, OwnerTenant: testTenant},
		scenarioResp: &querypb.EvaluateScenarioResponse{PortfolioId: secret, OwnerTenant: testTenant},
	}
	ts := serveScoped(t, fc, "PF1") // restricted to PF1; asks about PF2

	for _, route := range []struct{ name, method, path string }{
		{"exposure", http.MethodGet, "/v1/portfolios/PF2/exposure"},
		{"measures", http.MethodGet, "/v1/portfolios/PF2/measures"},
		{"scenario", http.MethodPost, "/v1/portfolios/PF2/scenario"},
	} {
		t.Run(route.name, func(t *testing.T) {
			var resp *http.Response
			var err error
			if route.method == http.MethodPost {
				resp, err = http.Post(ts.URL+route.path, "application/json", strings.NewReader(`{}`))
			} else {
				resp, err = http.Get(ts.URL + route.path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404 — the caller's token restricts them to PF1, the "+
					"portfolio list hides PF2 from them and its orders are refused, yet this route "+
					"served PF2 because it checks only the TENANT", resp.StatusCode)
			}
			if b := bodyOf(t, resp); strings.Contains(b, secret) {
				t.Errorf("the payload for a portfolio outside the caller's claim reached them: %s", b)
			}
		})
	}
}

// AND AN UNRESTRICTED CALLER IS UNAFFECTED. An empty claim PERMITS on a read
// path — auth.PortfolioInScope's documented contract — so a gate that refused
// here would be a service outage for every token that carries no restriction,
// which today is all of them.
func TestPortfolioRoutesStillServeAnUnrestrictedCaller(t *testing.T) {
	fc := &fakeClient{
		exposureResp: &querypb.ExposureResponse{
			PortfolioId: "PF2", OwnerTenant: testTenant,
			Set: &domainpb.ExposureSet{PortfolioId: "PF2"},
		},
	}
	ts := serveScoped(t, fc) // no portfolios claim at all

	resp, err := http.Get(ts.URL + "/v1/portfolios/PF2/exposure")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an empty claim asserts no restriction, and refusing it "+
			"would take the read surface down for every token issued today", resp.StatusCode)
	}
}

// A PORTFOLIO INSIDE THE CLAIM IS STILL SERVED. Without this the test above is
// satisfied by a gate that refuses everything.
func TestPortfolioRoutesServeAPortfolioInsideTheClaim(t *testing.T) {
	fc := &fakeClient{
		exposureResp: &querypb.ExposureResponse{
			PortfolioId: "PF1", OwnerTenant: testTenant,
			Set: &domainpb.ExposureSet{PortfolioId: "PF1"},
		},
	}
	ts := serveScoped(t, fc, "PF1", "PF3")

	resp, err := http.Get(ts.URL + "/v1/portfolios/PF1/exposure")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — PF1 is named in the caller's own claim", resp.StatusCode)
	}
}
