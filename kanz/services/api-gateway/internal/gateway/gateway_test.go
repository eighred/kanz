package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/gateway"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// testTenant is the tenant the harness authenticates as, AND the tenant every
// canned portfolio reply must claim to be owned by.
//
// The two are one value on purpose. writeOwned refuses any reply whose
// owner_tenant does not equal the caller's tenant (#222), so a fixture that
// omits it gets a 404 and the test reads as a transcoding failure. Before this
// gate existed all seven of these tests passed with no owner_tenant at all —
// which is exactly the hole: the responses were served to a caller whose tenant
// nobody had compared them against.
const testTenant = "t1"

// fakeClient is a querypb.RiskQueryServiceClient that records the last request
// and returns canned responses/errors.
type fakeClient struct {
	exposureResp *querypb.ExposureResponse
	measuresResp *querypb.MeasuresResponse
	scenarioResp *querypb.EvaluateScenarioResponse
	healthResp   *querypb.HealthResponse
	listResp     *querypb.ListPortfoliosResponse
	ordersResp   *orderpb.ListOrdersResponse
	err          error

	gotOrders *orderpb.ListOrdersRequest

	gotExposure *querypb.ExposureRequest
	gotMeasures *querypb.MeasuresRequest
	gotScenario *querypb.EvaluateScenarioRequest
}

func (f *fakeClient) Exposure(_ context.Context, in *querypb.ExposureRequest, _ ...grpc.CallOption) (*querypb.ExposureResponse, error) {
	f.gotExposure = in
	return f.exposureResp, f.err
}
func (f *fakeClient) ListOrders(_ context.Context, in *orderpb.ListOrdersRequest, _ ...grpc.CallOption) (*orderpb.ListOrdersResponse, error) {
	f.gotOrders = in
	return f.ordersResp, f.err
}
func (f *fakeClient) ListPortfolios(context.Context, *querypb.ListPortfoliosRequest, ...grpc.CallOption) (*querypb.ListPortfoliosResponse, error) {
	return f.listResp, f.err
}
func (f *fakeClient) Measures(_ context.Context, in *querypb.MeasuresRequest, _ ...grpc.CallOption) (*querypb.MeasuresResponse, error) {
	f.gotMeasures = in
	return f.measuresResp, f.err
}
func (f *fakeClient) EvaluateScenario(_ context.Context, in *querypb.EvaluateScenarioRequest, _ ...grpc.CallOption) (*querypb.EvaluateScenarioResponse, error) {
	f.gotScenario = in
	return f.scenarioResp, f.err
}
func (f *fakeClient) Health(_ context.Context, _ *querypb.HealthRequest, _ ...grpc.CallOption) (*querypb.HealthResponse, error) {
	return f.healthResp, f.err
}

// serve mounts the risk routes on the real capability router (SEC-M2) and authenticates in
// front of it, as middleware.Auth does in production — the router runs INSIDE the auth
// middleware, so a principal is always on ctx by the time a route is reached. These tests are
// about JSON↔proto transcoding; authorization itself is proven in internal/authz.
func serve(t *testing.T, fc *fakeClient) *httptest.Server {
	t.Helper()
	mux := authz.NewMux(authz.Grants{"analyst": {authz.Read}}, nil)
	gateway.New(fc, fc).Routes(mux)
	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := &middleware.Principal{Subject: "u1", Tenant: testTenant, Roles: []string{"analyst"}}
		mux.ServeHTTP(w, r.WithContext(middleware.WithPrincipal(r.Context(), p)))
	})
	ts := httptest.NewServer(authed)
	t.Cleanup(ts.Close)
	return ts
}

func TestExposureTranscodingParity(t *testing.T) {
	canned := &querypb.ExposureResponse{
		PortfolioId: "PF1",
		OwnerTenant: testTenant,
		AsOf:        timestamppb.New(mustTime("2026-05-20T12:00:00Z")),
		Set: &domainpb.ExposureSet{
			PortfolioId: "PF1",
			Exposures: []*domainpb.ExposureState{{
				PortfolioId: "PF1",
				Dimension:   domainpb.ExposureDimension_EXPOSURE_DIMENSION_CURRENCY,
				Bucket:      "USD",
				Gross:       &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 1000}, CurrencyCode: "USD"},
			}},
		},
		QualityFlags: []querypb.QualityFlag{querypb.QualityFlag_QUALITY_FLAG_DEGRADED},
	}
	fc := &fakeClient{exposureResp: canned}
	ts := serve(t, fc)

	resp, err := http.Get(ts.URL + "/v1/portfolios/PF1/exposure")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if fc.gotExposure.GetPortfolioId() != "PF1" {
		t.Errorf("upstream got portfolio_id %q", fc.gotExposure.GetPortfolioId())
	}
	// Parity: the REST JSON body must protojson-decode back into a message
	// equal to the canned proto the gRPC client returned.
	var got querypb.ExposureResponse
	body := readAll(t, resp)
	if err := protojson.Unmarshal(body, &got); err != nil {
		t.Fatalf("REST body not valid protojson: %v\n%s", err, body)
	}
	if !proto.Equal(&got, canned) {
		t.Errorf("REST↔gRPC parity broken:\n got %v\nwant %v", &got, canned)
	}
}

func TestMeasuresForwardsAsOfAndFilter(t *testing.T) {
	fc := &fakeClient{measuresResp: &querypb.MeasuresResponse{PortfolioId: "PF1", OwnerTenant: testTenant}}
	ts := serve(t, fc)

	resp, err := http.Get(ts.URL + "/v1/portfolios/PF1/measures?as_of=2026-05-20T12:00:00Z&measure=VaR99&measure=Delta")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := fc.gotMeasures.GetMeasures(); len(got) != 2 || got[0] != "VaR99" || got[1] != "Delta" {
		t.Errorf("measure filter not forwarded: %v", got)
	}
	if fc.gotMeasures.GetAsOf() == nil || !fc.gotMeasures.GetAsOf().AsTime().Equal(mustTime("2026-05-20T12:00:00Z")) {
		t.Errorf("as_of not forwarded: %v", fc.gotMeasures.GetAsOf())
	}
}

func TestScenarioDecodesBodyAndPathWins(t *testing.T) {
	fc := &fakeClient{scenarioResp: &querypb.EvaluateScenarioResponse{PortfolioId: "PF1", OwnerTenant: testTenant}}
	ts := serve(t, fc)

	body := `{"portfolio_id":"IGNORED","shocks":[{"parallelShift":{"pct":{"coefficient":"-5","exponent":-2}}}]}`
	resp, err := http.Post(ts.URL+"/v1/portfolios/PF1/scenario", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readAll(t, resp))
	}
	if fc.gotScenario.GetPortfolioId() != "PF1" {
		t.Errorf("path id should win over body: got %q", fc.gotScenario.GetPortfolioId())
	}
	if len(fc.gotScenario.GetShocks()) != 1 {
		t.Errorf("shock not decoded from body: %v", fc.gotScenario.GetShocks())
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		code codes.Code
		want int
	}{
		{codes.NotFound, http.StatusNotFound},
		{codes.InvalidArgument, http.StatusBadRequest},
		{codes.Unavailable, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		fc := &fakeClient{err: status.Error(tc.code, "x")}
		ts := serve(t, fc)
		resp, err := http.Get(ts.URL + "/v1/portfolios/PF1/exposure")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("gRPC %v ⇒ HTTP %d, want %d", tc.code, resp.StatusCode, tc.want)
		}
	}
}

func TestInvalidAsOfIs400(t *testing.T) {
	ts := serve(t, &fakeClient{exposureResp: &querypb.ExposureResponse{}})
	resp, err := http.Get(ts.URL + "/v1/portfolios/PF1/exposure?as_of=not-a-time")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestOpenAPIServed(t *testing.T) {
	rr := httptest.NewRecorder()
	gateway.OpenAPIHandler()(rr, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(rr.Body.String(), `"openapi"`) || !strings.Contains(rr.Body.String(), "/portfolios/{id}/exposure") {
		t.Error("openapi doc missing expected content")
	}
}

// THE LIST ROUTE'S TWO GATES (#399).
//
// GET /v1/portfolios answers "which portfolios may I look at" — the question
// every per-id route assumed the caller could already answer. It is gated twice,
// and the two gates answer different questions:
//
//   - the TENANT gate (writeOwned, shared with the per-id routes) refuses a
//     reply whose owner_tenant is not the caller's;
//   - the PORTFOLIO gate (auth.PortfolioInScope) drops rows the caller's claim
//     does not cover.
//
// The second is the one that is easy to get backwards, because the read and
// capital paths take OPPOSITE views of an absent claim, deliberately
// (pkg/auth/portfolio.go). Both directions are pinned below.

// serveAs is serve() with a principal the caller chooses, so the claim and the
// tenant can be varied independently.
func serveAs(t *testing.T, fc *fakeClient, p *middleware.Principal) *httptest.Server {
	t.Helper()
	mux := authz.NewMux(authz.Grants{"analyst": {authz.Read}}, nil)
	gateway.New(fc, fc).Routes(mux)
	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(middleware.WithPrincipal(r.Context(), p)))
	})
	ts := httptest.NewServer(authed)
	t.Cleanup(ts.Close)
	return ts
}

func listOf(t *testing.T, ts *httptest.Server) (int, []string) {
	t.Helper()
	res, err := http.Get(ts.URL + "/v1/portfolios")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return res.StatusCode, nil
	}
	var got struct {
		Portfolios []struct {
			PortfolioID string `json:"portfolio_id"`
		} `json:"portfolios"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	ids := make([]string, 0, len(got.Portfolios))
	for _, p := range got.Portfolios {
		ids = append(ids, p.PortfolioID)
	}
	return res.StatusCode, ids
}

func threePortfolios() *fakeClient {
	return &fakeClient{listResp: &querypb.ListPortfoliosResponse{
		OwnerTenant: testTenant,
		Portfolios: []*querypb.PortfolioSummary{
			{PortfolioId: "PF1"}, {PortfolioId: "PF2"}, {PortfolioId: "PF3"},
		},
	}}
}

// AN ABSENT CLAIM PERMITS, ON THIS PATH. The token asserted no portfolio
// restriction, so the tenant boundary is the operative limit. Reading it the
// other way — the capital path's rule — would show an unrestricted reader an
// EMPTY list, which they would read as "the fund has no portfolios" rather than
// as a permission problem, and would take to an operator as a data bug.
func TestAnUnrestrictedReaderSeesEveryPortfolioInTheTenant(t *testing.T) {
	ts := serveAs(t, threePortfolios(),
		&middleware.Principal{Subject: "u1", Tenant: testTenant, Roles: []string{"analyst"}})

	code, ids := listOf(t, ts)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(ids) != 3 {
		t.Fatalf("ids = %v, want all three.\n\n"+
			"An empty portfolios claim PERMITS on a read path (pkg/auth/portfolio.go). "+
			"Applying PortfolioEntitled's deny-on-empty here would hide the whole book.", ids)
	}
}

// A SCOPED READER SEES ONLY THEIR OWN. The claim is the allow-list, and the
// engine's reply is filtered against it before it is written.
func TestAScopedReaderSeesOnlyTheirClaimedPortfolios(t *testing.T) {
	ts := serveAs(t, threePortfolios(), &middleware.Principal{
		Subject: "u1", Tenant: testTenant, Roles: []string{"analyst"},
		Portfolios: []string{"PF2"},
	})

	code, ids := listOf(t, ts)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(ids) != 1 || ids[0] != "PF2" {
		t.Fatalf("ids = %v, want [PF2] — a list must not name portfolios the caller's claim "+
			"excludes, because it hands over ids they could not otherwise have guessed", ids)
	}
}

// THE TENANT GATE STILL APPLIES, and it matters MORE here than on a read: a
// caller naming a portfolio already knows its id.
func TestAListFromAnotherTenantIsRefused(t *testing.T) {
	fc := threePortfolios()
	fc.listResp.OwnerTenant = "someone-else"
	ts := serveAs(t, fc, &middleware.Principal{Subject: "u1", Tenant: testTenant, Roles: []string{"analyst"}})

	if code, _ := listOf(t, ts); code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — an engine serving another tenant must not have its "+
			"portfolio ids relayed", code)
	}
}

// EMPTY OWNER DENIES, the same deny-by-default stance the per-id routes take.
// An unconfigured engine tenant must fail closed, never open.
func TestAListWithNoOwnerTenantIsRefused(t *testing.T) {
	fc := threePortfolios()
	fc.listResp.OwnerTenant = ""
	ts := serveAs(t, fc, &middleware.Principal{Subject: "u1", Tenant: testTenant, Roles: []string{"analyst"}})

	if code, _ := listOf(t, ts); code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — a missing ownership signal is not permission", code)
	}
}

// ORDER HISTORY: THE MOST IDENTIFYING READ ON THIS SURFACE (#399).
//
// It names instruments, sizes and times, which is why it is the one
// portfolio-scoped route that consults the caller's portfolios claim as well as
// the tenant. The risk routes beside it check only the tenant; that asymmetry is
// deliberate and is argued at the handler.

func ordersServer(t *testing.T, fc *fakeClient, p *middleware.Principal) *httptest.Server {
	t.Helper()
	mux := authz.NewMux(authz.Grants{"analyst": {authz.Read}}, nil)
	gateway.New(fc, fc).Routes(mux)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(middleware.WithPrincipal(r.Context(), p)))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func getOrders(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	res, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func ordersFor(tenant string) *fakeClient {
	return &fakeClient{ordersResp: &orderpb.ListOrdersResponse{
		OwnerTenant: tenant,
		Orders:      []*orderpb.OrderState{{OrderId: "o1", PortfolioId: "PF1"}},
		Unindexed:   7,
	}}
}

func TestOrderHistoryIsReturnedWithItsUnindexedCount(t *testing.T) {
	fc := ordersFor(testTenant)
	ts := ordersServer(t, fc, &middleware.Principal{Subject: "u1", Tenant: testTenant, Roles: []string{"analyst"}})

	code, body := getOrders(t, ts, "/v1/portfolios/PF1/orders")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", code, body)
	}
	// THE COUNT MUST SURVIVE TRANSCODING. Without it the browser presents one
	// page as the portfolio's whole history — orders that predate migration 0007
	// cannot appear in any page at all.
	if !strings.Contains(body, `"unindexed":"7"`) && !strings.Contains(body, `"unindexed":7`) {
		t.Fatalf("the unindexed count did not reach the client: %s", body)
	}
	if fc.gotOrders.GetPortfolioId() != "PF1" {
		t.Fatalf("the OMS was asked for %q, want PF1", fc.gotOrders.GetPortfolioId())
	}
}

// A SCOPED CALLER CANNOT READ ANOTHER PORTFOLIO'S HISTORY, and is refused with
// the SAME 404 a missing portfolio gets — a distinct status would confirm that a
// portfolio they may not read nevertheless exists.
func TestAScopedCallerCannotReadAnotherPortfoliosHistory(t *testing.T) {
	fc := ordersFor(testTenant)
	ts := ordersServer(t, fc, &middleware.Principal{
		Subject: "u1", Tenant: testTenant, Roles: []string{"analyst"},
		Portfolios: []string{"PF2"},
	})

	code, _ := getOrders(t, ts, "/v1/portfolios/PF1/orders")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if fc.gotOrders != nil {
		t.Fatal("the OMS was queried for a portfolio the caller's claim excludes — the gate must " +
			"refuse before the read, or the history is fetched and then discarded")
	}
}

// AN UNRESTRICTED CALLER STILL READS IT. On a read path an absent claim asserts
// no restriction; applying the capital path's deny-on-empty here would hide
// every portfolio's history from every unscoped reader.
func TestAnUnrestrictedCallerReadsTheHistory(t *testing.T) {
	ts := ordersServer(t, ordersFor(testTenant),
		&middleware.Principal{Subject: "u1", Tenant: testTenant, Roles: []string{"analyst"}})

	if code, body := getOrders(t, ts, "/v1/portfolios/PF1/orders"); code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", code, body)
	}
}

// THE TENANT GATE STILL APPLIES: a history from an OMS serving another tenant is
// refused, through the same writeOwned every portfolio route uses.
func TestAHistoryFromAnotherTenantIsRefused(t *testing.T) {
	ts := ordersServer(t, ordersFor("someone-else"),
		&middleware.Principal{Subject: "u1", Tenant: testTenant, Roles: []string{"analyst"}})

	if code, _ := getOrders(t, ts, "/v1/portfolios/PF1/orders"); code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

// AN UNCONFIGURED OMS MEANS NO ROUTE AT ALL, not a route that always fails. An
// unregistered path says "not configured here", which is true; a registered one
// that errors says "broken", which is not.
func TestWithNoOMSTheHistoryRouteIsNotRegistered(t *testing.T) {
	mux := authz.NewMux(authz.Grants{"analyst": {authz.Read}}, nil)
	gateway.New(&fakeClient{}, nil).Routes(mux)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := &middleware.Principal{Subject: "u1", Tenant: testTenant, Roles: []string{"analyst"}}
		mux.ServeHTTP(w, r.WithContext(middleware.WithPrincipal(r.Context(), p)))
	}))
	defer ts.Close()

	if code, _ := getOrders(t, ts, "/v1/portfolios/PF1/orders"); code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with no OMS configured", code)
	}
}

// A BAD ?limit IS NOT A 400. It is a read that would otherwise have worked, and
// the OMS reads 0 as "your page size".
func TestAnUnparseableLimitFallsBackToTheServerPage(t *testing.T) {
	fc := ordersFor(testTenant)
	ts := ordersServer(t, fc, &middleware.Principal{Subject: "u1", Tenant: testTenant, Roles: []string{"analyst"}})

	if code, _ := getOrders(t, ts, "/v1/portfolios/PF1/orders?limit=banana"); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if fc.gotOrders.GetLimit() != 0 {
		t.Fatalf("limit = %d, want 0 (the server's own page size)", fc.gotOrders.GetLimit())
	}
}
