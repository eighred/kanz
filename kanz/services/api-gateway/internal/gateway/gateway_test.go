package gateway_test

import (
	"context"
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
	err          error

	gotExposure *querypb.ExposureRequest
	gotMeasures *querypb.MeasuresRequest
	gotScenario *querypb.EvaluateScenarioRequest
}

func (f *fakeClient) Exposure(_ context.Context, in *querypb.ExposureRequest, _ ...grpc.CallOption) (*querypb.ExposureResponse, error) {
	f.gotExposure = in
	return f.exposureResp, f.err
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
	gateway.New(fc).Routes(mux)
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
