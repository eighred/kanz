package tools

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/governed"
	"github.com/eighred/kanz/services/copilot/internal/llm"
	"github.com/eighred/kanz/services/copilot/internal/retrieval"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type unknownQualityEngine struct {
	querypb.UnimplementedRiskQueryServiceServer
	flag, measures, exposure, scenario atomic.Int32
}

func (s *unknownQualityEngine) measureSet() *domainpb.RiskMeasureSet {
	return &domainpb.RiskMeasureSet{Measures: []*domainpb.RiskMeasure{{Name: "Risk", Value: &commonpb.Decimal{Coefficient: 12345}, UncertaintyAbs: &commonpb.Decimal{Coefficient: 25, Exponent: -2}}}}
}
func (s *unknownQualityEngine) Measures(context.Context, *querypb.MeasuresRequest) (*querypb.MeasuresResponse, error) {
	s.measures.Add(1)
	return &querypb.MeasuresResponse{PortfolioId: "PF-T1", OwnerTenant: "t1", Set: s.measureSet(), QualityFlags: []querypb.QualityFlag{querypb.QualityFlag(s.flag.Load())}}, nil
}
func (s *unknownQualityEngine) Exposure(context.Context, *querypb.ExposureRequest) (*querypb.ExposureResponse, error) {
	s.exposure.Add(1)
	return &querypb.ExposureResponse{PortfolioId: "PF-T1", OwnerTenant: "t1", QualityFlags: []querypb.QualityFlag{querypb.QualityFlag(s.flag.Load())},
		Set: &domainpb.ExposureSet{Exposures: []*domainpb.ExposureState{{Dimension: domainpb.ExposureDimension_EXPOSURE_DIMENSION_CURRENCY, Bucket: "USD", Net: &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 12345}, CurrencyCode: "USD"}}}}}, nil
}
func (s *unknownQualityEngine) EvaluateScenario(context.Context, *querypb.EvaluateScenarioRequest) (*querypb.EvaluateScenarioResponse, error) {
	s.scenario.Add(1)
	return &querypb.EvaluateScenarioResponse{PortfolioId: "PF-T1", Projected: s.measureSet(), QualityFlags: []querypb.QualityFlag{querypb.QualityFlag(s.flag.Load())}}, nil
}

func TestGRPCCopilotUnknownQualityFlagWithholdsEveryToolAndRecovers(t *testing.T) {
	engine := &unknownQualityEngine{}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	querypb.RegisterRiskQueryServiceServer(grpcServer, engine)
	done := make(chan struct{})
	go func() { defer close(done); _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() { grpcServer.Stop(); <-done })
	conn, err := grpc.NewClient("passthrough:///unknown-quality", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	quiet := slog.New(slog.NewTextHandler(discard{}, nil))
	client := governed.NewGRPCClient(querypb.NewRiskQueryServiceClient(conn))
	registry := NewRegistry(auth.NewPolicyAuthorizer(testPolicy()), client, retrieval.IdentityCatalog{}, quiet)
	for _, tool := range []string{"get_risk_measures", "get_exposure", "evaluate_scenario"} {
		t.Run(tool, func(t *testing.T) {
			call := llm.ToolCall{Name: tool, Input: map[string]any{"portfolio_id": "PF-T1", "scenario": "-0.05"}}
			for _, flag := range []int32{5, -1, 1<<31 - 1, -1 << 31} {
				t.Run(fmt.Sprint(flag), func(t *testing.T) {
					engine.flag.Store(flag)
					var first string
					for attempt := range 2 {
						out := registry.Invoke(t.Context(), t1Analyst(), call)
						if out.IsError || out.Measured != 0 || out.Unavailable != 1 || len(out.Values) != 0 || !strings.Contains(out.Content, ": UNAVAILABLE") || strings.Contains(out.Content, " = 12345") || !strings.Contains(out.Content, "data quality: "+fmt.Sprint(flag)) {
							t.Fatalf("agent context/grounding retained an unknown-quality claim: %+v", out)
						}
						if attempt == 0 {
							first = out.Content
						} else if out.Content != first {
							t.Fatal("repeated response changed its refusal")
						}
					}
					beforeMeasures, beforeExposure, beforeScenario := engine.measures.Load(), engine.exposure.Load(), engine.scenario.Load()
					denied := registry.Invoke(t.Context(), t2Intruder(), call)
					if !denied.IsError || engine.measures.Load() != beforeMeasures || engine.scenario.Load() != beforeScenario || engine.exposure.Load() != beforeExposure+1 || len(denied.Values) != 0 || strings.Contains(denied.Content, "UNAVAILABLE") || strings.Contains(denied.Content, "data quality") {
						t.Fatalf("cross-tenant read reached domain data or leaked projection: %+v", denied)
					}
					engine.flag.Store(int32(querypb.QualityFlag_QUALITY_FLAG_DEGRADED))
					out := registry.Invoke(t.Context(), t1Analyst(), call)
					if out.IsError || out.Measured != 1 || out.Unavailable != 0 || len(out.Values) != 1 || out.Values[0] != 12345 || !strings.Contains(out.Content, " = 12345") || !strings.Contains(out.Content, "data quality: DEGRADED") {
						t.Fatalf("later known advisory response did not recover: %+v", out)
					}
				})
			}
		})
	}
}
