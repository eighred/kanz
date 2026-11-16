package tools

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/governed"
	"github.com/eighred/kanz/services/copilot/internal/llm"
	"github.com/eighred/kanz/services/copilot/internal/retrieval"
)

type fractionServer struct {
	querypb.UnimplementedRiskQueryServiceServer
	mu       sync.Mutex
	requests []*querypb.EvaluateScenarioRequest
}

func (*fractionServer) Exposure(context.Context, *querypb.ExposureRequest) (*querypb.ExposureResponse, error) {
	return &querypb.ExposureResponse{OwnerTenant: "t1", PortfolioId: "PF-T1"}, nil
}

func (s *fractionServer) EvaluateScenario(_ context.Context, r *querypb.EvaluateScenarioRequest) (*querypb.EvaluateScenarioResponse, error) {
	s.mu.Lock()
	s.requests = append(s.requests, proto.Clone(r).(*querypb.EvaluateScenarioRequest))
	s.mu.Unlock()
	return &querypb.EvaluateScenarioResponse{Projected: &domainpb.RiskMeasureSet{Measures: []*domainpb.RiskMeasure{
		{Name: "PnL", Value: &commonpb.Decimal{Coefficient: -25}},
	}}}, nil
}

func TestScenarioFractionGRPCToolBoundary(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := &fractionServer{}
	gs := grpc.NewServer()
	querypb.RegisterRiskQueryServiceServer(gs, srv)
	done := make(chan struct{})
	go func() { defer close(done); _ = gs.Serve(lis) }()
	t.Cleanup(func() { gs.Stop(); _ = lis.Close(); <-done })
	cc, err := grpc.NewClient("passthrough:///scenario-fraction", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	quiet := slog.New(slog.NewTextHandler(discard{}, nil))
	reg := NewRegistry(auth.NewPolicyAuthorizer(testPolicy()), governed.NewGRPCClient(querypb.NewRiskQueryServiceClient(cc)), retrieval.IdentityCatalog{}, quiet)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name  string
		input string
		want  *commonpb.Decimal
	}{
		{"tiny", "1e-7", &commonpb.Decimal{Coefficient: 1, Exponent: -7}},
		{"ordinary", "-0.05", &commonpb.Decimal{Coefficient: -5, Exponent: -2}},
		{"precise", "0.123456789012345678", &commonpb.Decimal{Coefficient: 123456789012345678, Exponent: -18}},
		{"nonfinite", "NaN", nil},
		{"unrepresentable", "0.12345678901234567891", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv.mu.Lock()
			before := len(srv.requests)
			srv.mu.Unlock()
			out := reg.Invoke(ctx, t1Analyst(), llm.ToolCall{Name: "evaluate_scenario", Input: map[string]any{"portfolio_id": "PF-T1", "scenario": tc.input}})
			srv.mu.Lock()
			defer srv.mu.Unlock()
			if tc.want == nil {
				if !out.IsError || len(srv.requests) != before || strings.Contains(out.Content, "PnL =") || len(out.Values) != 0 {
					t.Fatalf("invalid fraction queried risk or stated a result: %+v requests=%d", out, len(srv.requests)-before)
				}
				return
			}
			if out.IsError || len(srv.requests) != before+1 || !strings.Contains(out.Content, "PnL = -25") || out.Measured != 1 {
				t.Fatalf("authorized scenario failed: %+v requests=%d", out, len(srv.requests)-before)
			}
			r := srv.requests[before]
			if r.GetPortfolioId() != "PF-T1" || len(r.GetShocks()) != 1 || !proto.Equal(r.GetShocks()[0].GetParallelShift().GetPct(), tc.want) {
				t.Fatalf("generated wire changed the authorized fraction: %v want %v", r, tc.want)
			}
		})
	}
	t.Run("foreign_tenant", func(t *testing.T) {
		srv.mu.Lock()
		before := len(srv.requests)
		srv.mu.Unlock()
		out := reg.Invoke(ctx, t2Intruder(), llm.ToolCall{Name: "evaluate_scenario", Input: map[string]any{"portfolio_id": "PF-T1", "scenario": "-0.05"}})
		srv.mu.Lock()
		defer srv.mu.Unlock()
		if !out.IsError || len(srv.requests) != before || len(out.Values) != 0 {
			t.Fatalf("foreign tenant reached the scenario: %+v", out)
		}
	})
}
