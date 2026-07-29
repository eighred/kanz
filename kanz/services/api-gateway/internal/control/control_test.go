package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	operatorpb "github.com/eighred/kanz/kanz-schemas-go/operator/v1"

	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// stubClient satisfies OperatorServiceClient by embedding the interface: any method
// a test does not override panics rather than silently returning a zero value, so a
// route wired to the wrong RPC fails loudly instead of passing.
type stubClient struct {
	operatorpb.OperatorServiceClient

	setRegion   func(*operatorpb.SetNodeRegionRequest) (*operatorpb.SetNodeRegionResponse, error)
	setVenue    func(*operatorpb.SetVenueKeysRequest) (*operatorpb.SetVenueKeysResponse, error)
	drainCalls  []string
	addNodeErr  error
	testConnErr error
}

func (s *stubClient) TestConnection(_ context.Context, _ *operatorpb.TestConnectionRequest, _ ...grpc.CallOption) (*operatorpb.TestConnectionResponse, error) {
	if s.testConnErr != nil {
		return nil, s.testConnErr
	}
	return &operatorpb.TestConnectionResponse{Reachable: true}, nil
}

func (s *stubClient) SetNodeRegion(_ context.Context, in *operatorpb.SetNodeRegionRequest, _ ...grpc.CallOption) (*operatorpb.SetNodeRegionResponse, error) {
	return s.setRegion(in)
}

func (s *stubClient) SetVenueKeys(_ context.Context, in *operatorpb.SetVenueKeysRequest, _ ...grpc.CallOption) (*operatorpb.SetVenueKeysResponse, error) {
	return s.setVenue(in)
}

func (s *stubClient) Drain(_ context.Context, in *operatorpb.DrainRequest, _ ...grpc.CallOption) (*operatorpb.DrainResponse, error) {
	s.drainCalls = append(s.drainCalls, in.GetName())
	return &operatorpb.DrainResponse{}, nil
}

func (s *stubClient) AddNode(_ context.Context, _ *operatorpb.AddNodeRequest, _ ...grpc.CallOption) (*operatorpb.AddNodeResponse, error) {
	if s.addNodeErr != nil {
		return nil, s.addNodeErr
	}
	return &operatorpb.AddNodeResponse{ProvisionId: "prov-1"}, nil
}

// serve builds the real authz.Mux with the real grants, so these tests exercise the
// capability enforcement rather than a reimplementation of it.
func serve(t *testing.T, c operatorpb.OperatorServiceClient, roles []string, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	m := authz.NewMux(authz.Grants{
		"kanz-user":     {authz.Read},
		"kanz-trader":   {authz.Read, authz.Trade},
		"kanz-operator": {authz.Read, authz.Operate},
	})
	New(c, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes(m)

	ctx := middleware.WithPrincipal(req.Context(), &middleware.Principal{
		Subject: "someone", Tenant: "fund-alpha", Roles: roles,
	})
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

// THE CENTRAL PROPERTY. Reading the fund and trading on it must not carry authority
// over the estate that executes those trades.
func TestControlRoutesRefuseReadAndTradeRoles(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
	}{
		{"baseline reader", []string{"kanz-user"}},
		{"trader", []string{"kanz-trader"}},
		{"both, still not an operator", []string{"kanz-user", "kanz-trader"}},
		{"no roles at all", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(t, &stubClient{}, tc.roles,
				httptest.NewRequest("POST", "/v1/control/nodes/node-1/drain", nil))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("draining a node was reachable with roles %v (status %d). Operating the "+
					"estate is a different authority from reading or trading — a trader who can "+
					"drain nodes can take the platform down.", tc.roles, rec.Code)
			}
		})
	}
}

func TestControlRoutesAdmitTheOperatorRole(t *testing.T) {
	c := &stubClient{}
	rec := serve(t, c, []string{"kanz-operator"},
		httptest.NewRequest("POST", "/v1/control/nodes/node-1/drain", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("operator role was refused: status %d body %s", rec.Code, rec.Body)
	}
	if len(c.drainCalls) != 1 || c.drainCalls[0] != "node-1" {
		t.Fatalf("node name must come from the path; drain calls = %v", c.drainCalls)
	}
}

// The node is addressed by the PATH. A body that disagrees must not win, or an
// operator drains the node they were not looking at.
func TestPathIdentityOverridesTheBody(t *testing.T) {
	var got *operatorpb.SetNodeRegionRequest
	c := &stubClient{setRegion: func(in *operatorpb.SetNodeRegionRequest) (*operatorpb.SetNodeRegionResponse, error) {
		got = in
		return &operatorpb.SetNodeRegionResponse{}, nil
	}}
	body := strings.NewReader(`{"name":"a-different-node","region":"asia"}`)
	rec := serve(t, c, []string{"kanz-operator"},
		httptest.NewRequest("POST", "/v1/control/nodes/node-1/region", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if got.GetName() != "node-1" {
		t.Errorf("path must win: relabelled %q, but the URL said node-1", got.GetName())
	}
	if got.GetRegion() != "asia" {
		t.Errorf("non-identity fields still come from the body; region = %q", got.GetRegion())
	}
}

func TestVenueIdentityComesFromThePath(t *testing.T) {
	var got *operatorpb.SetVenueKeysRequest
	c := &stubClient{setVenue: func(in *operatorpb.SetVenueKeysRequest) (*operatorpb.SetVenueKeysResponse, error) {
		got = in
		return &operatorpb.SetVenueKeysResponse{ExchangeAccountId: "12345"}, nil
	}}
	body := strings.NewReader(`{"venue":"okx","apiKey":"k","apiSecret":"s"}`)
	rec := serve(t, c, []string{"kanz-operator"},
		httptest.NewRequest("PUT", "/v1/control/venues/binance/keys", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if got.GetVenue() != "binance" {
		t.Errorf("path must win: wrote keys for %q, but the URL said binance. Writing a "+
			"credential to the wrong venue points an adapter at another exchange's account.",
			got.GetVenue())
	}
	if got.GetApiKey() != "k" || got.GetApiSecret() != "s" {
		t.Error("the credential itself must still come from the body")
	}
}

// An unknown field means the caller is on a different contract. Dropping it silently
// would provision something other than what was asked for.
func TestUnknownFieldIsRejected(t *testing.T) {
	body := strings.NewReader(`{"hostname":"h","ip":"10.0.0.1","notAField":true}`)
	rec := serve(t, &stubClient{}, []string{"kanz-operator"},
		httptest.NewRequest("POST", "/v1/control/nodes", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown request field was accepted (status %d). protojson must not "+
			"DiscardUnknown here.", rec.Code)
	}
}

// Unimplemented is what the operator returns for a capability that is deliberately
// unconfigured (no provisioner image, no secret backend). Reporting it as 500 sends
// someone hunting a bug that is a missing env var.
func TestUnconfiguredCapabilityIsNotAServerFault(t *testing.T) {
	c := &stubClient{addNodeErr: status.Error(codes.Unimplemented, "AddNode is not configured")}
	rec := serve(t, c, []string{"kanz-operator"},
		httptest.NewRequest("POST", "/v1/control/nodes", strings.NewReader(`{"hostname":"h"}`)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("want 501 for a deliberately unconfigured capability, got %d", rec.Code)
	}
}

// S4b: the exchange rejected the credential. That is the caller's problem to fix and
// the message is already sanitized server-side, so it must reach them intact.
func TestExchangeRejectionReachesTheCaller(t *testing.T) {
	c := &stubClient{setVenue: func(*operatorpb.SetVenueKeysRequest) (*operatorpb.SetVenueKeysResponse, error) {
		return nil, status.Error(codes.FailedPrecondition, "exchange rejected the credentials")
	}}
	rec := serve(t, c, []string{"kanz-operator"},
		httptest.NewRequest("PUT", "/v1/control/venues/binance/keys",
			strings.NewReader(`{"apiKey":"bad","apiSecret":"bad"}`)))
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("want 412, got %d", rec.Code)
	}
	var out map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("undecodable error body: %v", err)
	}
	if !strings.Contains(out["error"], "exchange rejected") {
		t.Errorf("the exchange's verdict must reach the operator; got %q", out["error"])
	}
}

// AN Internal MESSAGE IS THE INNERMOST LAYER'S VERDICT AND MUST REACH THE CALLER.
//
// The four bounds on a Test Connection are ordered so the operator's probe wait fires
// first, because only it can say the probe Job ran and did not answer. Erasing its message
// here half-nullifies that: the layers fire in the right order and then the payload of the
// layer that knows the most is replaced by "control plane error", which teaches an operator
// staring at the Add Node form precisely nothing.
func TestAnInternalFaultsExplanationReachesTheCaller(t *testing.T) {
	const detail = "probe job probe-abc did not complete in time (waited 60s)"
	c := &stubClient{testConnErr: status.Error(codes.Internal, detail)}
	rec := serve(t, c, []string{"kanz-operator"},
		httptest.NewRequest("POST", "/v1/control/test-connection", strings.NewReader(`{"ip":"10.0.0.5"}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 for an Internal fault, got %d", rec.Code)
	}
	var out map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("undecodable error body: %v", err)
	}
	if !strings.Contains(out["error"], detail) {
		t.Errorf("the operator's own explanation was dropped; got %q, want it to contain %q",
			out["error"], detail)
	}
}

// An Internal status with no message must still read as a server fault rather than as an
// empty string the caller has to guess at.
func TestAnInternalFaultWithNoMessageStillSaysSomething(t *testing.T) {
	c := &stubClient{testConnErr: status.Error(codes.Internal, "")}
	rec := serve(t, c, []string{"kanz-operator"},
		httptest.NewRequest("POST", "/v1/control/test-connection", strings.NewReader(`{"ip":"10.0.0.5"}`)))
	var out map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("undecodable error body: %v", err)
	}
	if out["error"] != "control plane error" {
		t.Errorf("got %q, want the bare fallback", out["error"])
	}
}

// Every route on this surface is Operate. A future route added without thinking
// about it should trip this rather than inherit Read by accident.
func TestEveryControlRouteDemandsOperate(t *testing.T) {
	m := authz.NewMux(authz.Grants{})
	New(&stubClient{}, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes(m)
	routes := m.Routes()
	if len(routes) == 0 {
		t.Fatal("no routes registered — this guard would assert nothing")
	}
	for _, rt := range routes {
		if rt.Capability != authz.Operate {
			t.Errorf("%s requires %q; every control-plane route must require %q",
				rt.Pattern, rt.Capability, authz.Operate)
		}
		if !strings.Contains(rt.Pattern, "/v1/control/") {
			t.Errorf("%s is not under /v1/control/ — the control surface must stay one prefix "+
				"so a policy can be written about it", rt.Pattern)
		}
	}
}

// deadlineSeen records the budget the handler put on the outbound RPC. The gateway is
// the middle of three layers bounding a Test Connection, and the only observable it has
// is the context it hands the client.
type deadlineSeen struct {
	operatorpb.OperatorServiceClient
	testConn time.Duration
	listed   time.Duration
}

func (d *deadlineSeen) TestConnection(ctx context.Context, _ *operatorpb.TestConnectionRequest, _ ...grpc.CallOption) (*operatorpb.TestConnectionResponse, error) {
	d.testConn = budgetOf(ctx)
	return &operatorpb.TestConnectionResponse{Reachable: true, LatencyMs: 3}, nil
}

func (d *deadlineSeen) ListNodes(ctx context.Context, _ *operatorpb.ListNodesRequest, _ ...grpc.CallOption) (*operatorpb.ListNodesResponse, error) {
	d.listed = budgetOf(ctx)
	return &operatorpb.ListNodesResponse{}, nil
}

// budgetOf rounds the remaining time up to whole seconds: the deadline is set a few
// microseconds before the client sees it, and the assertion is about which budget was
// chosen, not about scheduling noise.
func budgetOf(ctx context.Context) time.Duration {
	dl, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	return time.Until(dl).Round(time.Second)
}

// TestTestConnectionGetsTheLongBudgetAndOtherRoutesDoNot is the executable half of the
// deadline hierarchy at this layer. The operator answers TestConnection by running a
// probe Job and waiting for it, so a 30s gateway budget would cancel a healthy probe and
// report "control plane did not answer in time" — the gateway blaming itself for work it
// simply did not wait for. The other ten routes must NOT inherit the long budget: they
// are reads, and a slow read is a control plane that is not answering.
func TestTestConnectionGetsTheLongBudgetAndOtherRoutesDoNot(t *testing.T) {
	c := &deadlineSeen{}
	rec := serve(t, c, []string{"kanz-operator"},
		httptest.NewRequest("POST", "/v1/control/test-connection", strings.NewReader(`{"ip":"10.0.0.5"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if c.testConn != testConnectionTimeout {
		t.Errorf("TestConnection ran with a %s budget, want %s — the operator's own probe wait is %s, "+
			"and a gateway budget below it makes the operator's verdict unreachable",
			c.testConn, testConnectionTimeout, 60*time.Second)
	}

	rec = serve(t, c, []string{"kanz-operator"}, httptest.NewRequest("GET", "/v1/control/nodes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if c.listed != callTimeout {
		t.Errorf("ListNodes ran with a %s budget, want %s — only the probing route may wait longer",
			c.listed, callTimeout)
	}
}
