package stateread_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/agentgate"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/mcp/internal/riskread"
	"github.com/eighred/kanz/services/mcp/internal/server"
	"github.com/eighred/kanz/services/mcp/internal/stateread"
)

// Real generated gRPC transport plus the production MCP handler/authorizer.
// Domain fixtures are synthetic; actual OMS storage/mTLS have separate tests.
type stateDomain struct {
	querypb.UnimplementedRiskQueryServiceServer
	querypb.UnimplementedPositionQueryServiceServer
	querypb.UnimplementedOrderStateReadServiceServer
	exposures, positions, orders atomic.Int32
	page                         *querypb.PositionsResponse
	row                          *querypb.OrderStateReadResponse
}

func (s *stateDomain) Exposure(_ context.Context, req *querypb.ExposureRequest) (*querypb.ExposureResponse, error) {
	s.exposures.Add(1)
	return &querypb.ExposureResponse{PortfolioId: req.GetPortfolioId(), OwnerTenant: "t1"}, nil
}
func (s *stateDomain) Positions(context.Context, *querypb.PositionsRequest) (*querypb.PositionsResponse, error) {
	s.positions.Add(1)
	return proto.Clone(s.page).(*querypb.PositionsResponse), nil
}
func (s *stateDomain) OrderStateRead(context.Context, *querypb.OrderStateReadRequest) (*querypb.OrderStateReadResponse, error) {
	s.orders.Add(1)
	return proto.Clone(s.row).(*querypb.OrderStateReadResponse), nil
}

type decisionCapture struct{ entries []*observationpb.DecisionLog }

func (r *decisionCapture) Record(_ context.Context, entry *observationpb.DecisionLog) error {
	r.entries = append(r.entries, proto.Clone(entry).(*observationpb.DecisionLog))
	return nil
}

func stateBoundary(t *testing.T) (*server.Server, *stateDomain, *decisionCapture, *stateread.Client) {
	t.Helper()
	stamp := timestamppb.New(time.Now().UTC())
	q := &querypb.ObservedQuantity{Knowledge: querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_SUPPORTED, Value: &commonpb.Decimal{Coefficient: 9007199254740993, Exponent: -11}}
	domain := &stateDomain{page: &querypb.PositionsResponse{PortfolioId: "P", OwnerTenant: "t1", AsOf: stamp, SnapshotId: strings.Repeat("a", 64), TotalObserved: 1,
		Positions: []*querypb.ObservedPosition{{InstrumentId: "I", Quantity: q, AsOf: stamp}}},
		row: &querypb.OrderStateReadResponse{PortfolioId: "P", OwnerTenant: "t1", OrderId: "O", InstrumentId: "I", RecordedStatus: "ORDER_STATUS_ROUTED",
			StatusKnowledge: querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_SUPPORTED, OrderedQuantity: q, AsOf: stamp, ReadAt: stamp}}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	querypb.RegisterRiskQueryServiceServer(grpcServer, domain)
	querypb.RegisterPositionQueryServiceServer(grpcServer, domain)
	querypb.RegisterOrderStateReadServiceServer(grpcServer, domain)
	done := make(chan struct{})
	go func() { defer close(done); _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() { grpcServer.Stop(); <-done })
	conn, err := grpc.NewClient("passthrough:///state-fixture", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	reader := riskread.NewScoped(querypb.NewRiskQueryServiceClient(conn), "t1")
	stateReader, err := stateread.New(querypb.NewPositionQueryServiceClient(conn), querypb.NewOrderStateReadServiceClient(conn), "t1")
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	recorder := &decisionCapture{}
	policy := &auth.Policy{Roles: map[string][]auth.Action{"reader": {auth.ActionRiskRead, auth.Action("order.read")}, "risk-only": {auth.ActionRiskRead}}}
	authorizer := auth.NewAuditedAuthorizer(auth.NewPolicyAuthorizer(policy), recorder, "state-read-fixture", quiet)
	ready := &server.Readiness{}
	ready.Set(true)
	return server.New(ready, agentgate.NewGate(authorizer, reader, quiet), reader, quiet, server.WithStateReads(stateReader)), domain, recorder, stateReader
}

func stateRPC(t *testing.T, handler http.Handler, body, tenant, role string, portfolios ...string) (int, string, bool) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if tenant != "anonymous" {
		auth.SetPrincipalHeaders(req.Header, "dummy-reader", tenant, []string{role})
		if err := auth.SetPrincipalPortfolios(req.Header, portfolios); err != nil {
			t.Fatal(err)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code == http.StatusUnauthorized {
		return response.Code, response.Body.String(), true
	}
	var rpc struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &rpc); err != nil {
		t.Fatal(err)
	}
	if rpc.Error != nil {
		return response.Code, rpc.Error.Message, true
	}
	if len(rpc.Result.Content) > 0 {
		return response.Code, rpc.Result.Content[0].Text, rpc.Result.IsError
	}
	return response.Code, response.Body.String(), false
}

const positionCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_positions","arguments":{"portfolio_id":"P"}}}`
const orderStateCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_order_state","arguments":{"portfolio_id":"P","order_id":"O"}}}`

func TestStateToolsCheckTenantAndPortfolioBeforeLookupAndAuditDenials(t *testing.T) {
	handler, domain, recorder, _ := stateBoundary(t)
	for _, tenant := range []string{"t2", "t1"} {
		portfolio := "P"
		if tenant == "t1" {
			portfolio = "other"
		}
		_, _, refused := stateRPC(t, handler, positionCall, tenant, "reader", portfolio)
		if !refused {
			t.Fatal("unentitled caller was allowed")
		}
	}
	if domain.exposures.Load() != 0 || domain.positions.Load() != 0 || domain.orders.Load() != 0 {
		t.Fatal("denied caller reached a domain lookup")
	}
	if len(recorder.entries) != 2 {
		t.Fatalf("denied attempts bypassed canonical decision recording: %d", len(recorder.entries))
	}
	if code, _, _ := stateRPC(t, handler, positionCall, "anonymous", ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous HTTP status: %d", code)
	}
	if domain.exposures.Load() != 0 {
		t.Fatal("anonymous request looked up portfolio existence")
	}
}

func TestStateToolsPreserveExactValuesAndRequireDistinctOrderReadGrant(t *testing.T) {
	handler, domain, _, _ := stateBoundary(t)
	_, positions, refused := stateRPC(t, handler, positionCall, "t1", "reader", "P")
	if refused || !strings.Contains(positions, `"coefficient":"9007199254740993"`) || !strings.Contains(positions, `"scope":"OBSERVED_RISK_STATE"`) {
		t.Fatalf("position projection: %s", positions)
	}
	_, orderState, refused := stateRPC(t, handler, orderStateCall, "t1", "reader", "P")
	if refused || !strings.Contains(orderState, `"coefficient":"9007199254740993"`) || !strings.Contains(orderState, `"scope":"RECORDED_OMS_STATE"`) {
		t.Fatalf("order-state projection: %s", orderState)
	}
	before := domain.orders.Load()
	_, _, refused = stateRPC(t, handler, orderStateCall, "t1", "risk-only", "P")
	if !refused || domain.orders.Load() != before {
		t.Fatal("risk.read granted order.read implicitly")
	}
}

func TestStateToolsRebindResponseOwnershipAndWithholdStaleOrUnknownValues(t *testing.T) {
	handler, domain, _, _ := stateBoundary(t)
	domain.page.OwnerTenant = "t2"
	domain.page.Positions[0].InstrumentId = "private-other-tenant"
	_, text, refused := stateRPC(t, handler, positionCall, "t1", "reader", "P")
	if !refused || strings.Contains(text, "private-") || strings.Contains(text, "t2") {
		t.Fatalf("post-gate owner substitution disclosed data: %s", text)
	}
	domain.page.OwnerTenant = "t1"
	domain.page.Positions[0].InstrumentId = "I"
	domain.page.QualityFlags = []querypb.QualityFlag{querypb.QualityFlag_QUALITY_FLAG_STALE}
	_, text, refused = stateRPC(t, handler, positionCall, "t1", "reader", "P")
	if refused || strings.Contains(text, "9007199254740993") || !strings.Contains(text, `"status":"UNKNOWN"`) {
		t.Fatalf("stale value became an agent number: %s", text)
	}
	domain.page.QualityFlags = nil
	domain.page.Positions[0].Quantity.Knowledge = querypb.StateReadKnowledge(999)
	_, text, refused = stateRPC(t, handler, positionCall, "t1", "reader", "P")
	if refused || strings.Contains(text, "9007199254740993") || !strings.Contains(text, `"status":"UNKNOWN"`) {
		t.Fatalf("unknown knowledge became an agent number: %s", text)
	}
	domain.row.OwnerTenant = "t2"
	_, text, refused = stateRPC(t, handler, orderStateCall, "t1", "reader", "P")
	if !refused || strings.Contains(text, "9007199254740993") {
		t.Fatalf("order owner substitution accepted: %s", text)
	}
}

func TestStateToolsRefuseUndeclaredArgumentsAndOversizedPages(t *testing.T) {
	handler, domain, _, _ := stateBoundary(t)
	for _, body := range []string{strings.Replace(positionCall, `"portfolio_id":"P"`, `"portfolio_id":"P","as_of":"2020-01-01"`, 1), strings.Replace(positionCall, `"portfolio_id":"P"`, `"portfolio_id":"P","page_size":101`, 1), strings.Replace(orderStateCall, `"order_id":"O"`, `"order_id":"O","page_token":"extra"`, 1)} {
		_, _, refused := stateRPC(t, handler, body, "t1", "reader", "P")
		if !refused {
			t.Fatalf("unsupported argument accepted: %s", body)
		}
	}
	if domain.exposures.Load() != 0 || domain.positions.Load() != 0 || domain.orders.Load() != 0 {
		t.Fatal("invalid arguments reached a domain backend")
	}
}
