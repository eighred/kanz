package gateway_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
)

// The risk read routes authorized on ROLE ALONE (#222).
//
// authz.Mux checks the caller holds the capability; it has no notion of which
// RESOURCE the capability applies to. Every authenticated caller holds Read
// (API_GATEWAY_REQUIRED_ROLE is kanz-user), the portfolio id came straight from
// r.PathValue, and the principal was never consulted — so any authenticated user
// could read any portfolio's exposure, measures or scenario by naming its id.
//
// The risk engine has stamped owner_tenant on the reply since WIRE-02a and
// nothing read it. These tests are that read.
//
// Coverage note: all THREE portfolio routes, deliberately. EvaluateScenarioResponse
// had no owner_tenant field at all until this change, so a gate written over the
// two that had it would have left POST /scenario open while the route table looked
// complete — and a what-if projection discloses the same portfolio risk as the
// exposure it derives from.

func bodyOf(t *testing.T, r *http.Response) string {
	t.Helper()
	defer func() { _ = r.Body.Close() }()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// hitAll drives each portfolio-scoped route once and hands back the response.
func hitAll(t *testing.T, fc *fakeClient) map[string]*http.Response {
	t.Helper()
	ts := serve(t, fc)
	out := map[string]*http.Response{}

	r, err := http.Get(ts.URL + "/v1/portfolios/PF1/exposure")
	if err != nil {
		t.Fatal(err)
	}
	out["exposure"] = r

	r, err = http.Get(ts.URL + "/v1/portfolios/PF1/measures")
	if err != nil {
		t.Fatal(err)
	}
	out["measures"] = r

	r, err = http.Post(ts.URL+"/v1/portfolios/PF1/scenario", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	out["scenario"] = r
	return out
}

// A reply owned by ANOTHER tenant must not reach the caller, on any route.
func TestPortfolioRoutesRefuseAnotherTenantsData(t *testing.T) {
	const secret = "SUPER-SECRET-INSTRUMENT"
	fc := &fakeClient{
		exposureResp: &querypb.ExposureResponse{
			PortfolioId: "PF1", OwnerTenant: "someone-else",
			Set: &domainpb.ExposureSet{PortfolioId: secret},
		},
		measuresResp: &querypb.MeasuresResponse{PortfolioId: secret, OwnerTenant: "someone-else"},
		scenarioResp: &querypb.EvaluateScenarioResponse{PortfolioId: secret, OwnerTenant: "someone-else"},
	}

	for route, resp := range hitAll(t, fc) {
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 — a portfolio owned by another tenant was served "+
				"to a caller authenticated as %q", route, resp.StatusCode, testTenant)
		}
		if b := bodyOf(t, resp); strings.Contains(b, secret) {
			t.Errorf("%s: the upstream payload reached the client despite the refusal: %s", route, b)
		}
	}
}

// An EMPTY owner_tenant denies. query.v1's own contract: "Empty ⇒ the engine has
// no ownership record for the portfolio, which a deny-by-default gate must treat
// as a denial, not an allow." A missing signal is not permission — and it is the
// state every reply was in before this change.
func TestPortfolioRoutesDenyWhenOwnerTenantIsEmpty(t *testing.T) {
	fc := &fakeClient{
		exposureResp: &querypb.ExposureResponse{PortfolioId: "PF1"},
		measuresResp: &querypb.MeasuresResponse{PortfolioId: "PF1"},
		scenarioResp: &querypb.EvaluateScenarioResponse{PortfolioId: "PF1"},
	}
	for route, resp := range hitAll(t, fc) {
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 on an empty owner_tenant — an unstamped reply "+
				"must not be treated as permission", route, resp.StatusCode)
		}
	}
}

// The caller's OWN portfolio still works. Without this the guard above is
// satisfied by a gateway that 404s everything.
func TestPortfolioRoutesServeTheCallersOwnTenant(t *testing.T) {
	fc := &fakeClient{
		exposureResp: &querypb.ExposureResponse{PortfolioId: "PF1", OwnerTenant: testTenant},
		measuresResp: &querypb.MeasuresResponse{PortfolioId: "PF1", OwnerTenant: testTenant},
		scenarioResp: &querypb.EvaluateScenarioResponse{PortfolioId: "PF1", OwnerTenant: testTenant},
	}
	for route, resp := range hitAll(t, fc) {
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 for the caller's own tenant", route, resp.StatusCode)
		}
	}
}

// NO ORACLE. "Exists but is not yours" and "does not exist" must be
// indistinguishable — status AND body. If they differ, iterating ids enumerates
// which portfolios exist in other tenants, which is the thing isolation exists
// to hide. Body parity is the half that is easy to miss: the engine's own
// NotFound message would otherwise come through verbatim.
func TestCrossTenantIsIndistinguishableFromNotFound(t *testing.T) {
	foreign := hitAll(t, &fakeClient{
		exposureResp: &querypb.ExposureResponse{PortfolioId: "PF1", OwnerTenant: "someone-else"},
		measuresResp: &querypb.MeasuresResponse{PortfolioId: "PF1", OwnerTenant: "someone-else"},
		scenarioResp: &querypb.EvaluateScenarioResponse{PortfolioId: "PF1", OwnerTenant: "someone-else"},
	})
	absent := hitAll(t, &fakeClient{
		err: status.Error(codes.NotFound, "portfolio PF1 is unknown to this engine"),
	})

	for route := range foreign {
		fs, as := foreign[route].StatusCode, absent[route].StatusCode
		fb, ab := bodyOf(t, foreign[route]), bodyOf(t, absent[route])
		if fs != as {
			t.Errorf("%s: cross-tenant status %d != not-found status %d — the status code alone "+
				"enumerates other tenants' portfolios", route, fs, as)
		}
		if fb != ab {
			t.Errorf("%s: bodies differ, so the response still distinguishes "+
				"\"not yours\" from \"not there\":\n  cross-tenant: %s\n  not-found:    %s",
				route, fb, ab)
		}
		if strings.Contains(ab, "unknown to this engine") {
			t.Errorf("%s: the engine's own not-found wording reached the client (%s) — upstream "+
				"detail on a portfolio-scoped 404 is itself the oracle", route, ab)
		}
	}
}
