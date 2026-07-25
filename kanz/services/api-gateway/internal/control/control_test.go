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

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	operatorpb "github.com/kanz-eng/kanz-schemas-go/operator/v1"

	"github.com/kanz-eng/kanz/services/api-gateway/internal/authz"
	"github.com/kanz-eng/kanz/services/api-gateway/internal/middleware"
)

// stubClient satisfies OperatorServiceClient by embedding the interface: any method
// a test does not override panics rather than silently returning a zero value, so a
// route wired to the wrong RPC fails loudly instead of passing.
type stubClient struct {
	operatorpb.OperatorServiceClient

	setRegion  func(*operatorpb.SetNodeRegionRequest) (*operatorpb.SetNodeRegionResponse, error)
	setVenue   func(*operatorpb.SetVenueKeysRequest) (*operatorpb.SetVenueKeysResponse, error)
	drainCalls []string
	addNodeErr error
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
