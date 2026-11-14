package riskread

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eighred/kanz/internal/agentgate"
	"github.com/eighred/kanz/internal/measureread"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/mcp/internal/server"
)

type unknownFlagEngine struct {
	querypb.UnimplementedRiskQueryServiceServer
	flag, reads atomic.Int32
}

func (s *unknownFlagEngine) Exposure(context.Context, *querypb.ExposureRequest) (*querypb.ExposureResponse, error) {
	return &querypb.ExposureResponse{PortfolioId: "PF-1", OwnerTenant: "t1"}, nil
}

func (s *unknownFlagEngine) Measures(context.Context, *querypb.MeasuresRequest) (*querypb.MeasuresResponse, error) {
	s.reads.Add(1)
	return &querypb.MeasuresResponse{PortfolioId: "PF-1", OwnerTenant: "t1",
		QualityFlags: []querypb.QualityFlag{querypb.QualityFlag(s.flag.Load())},
		Set:          &domainpb.RiskMeasureSet{Measures: []*domainpb.RiskMeasure{{Name: "VaR99", Value: dec(12345, 0), UncertaintyAbs: dec(25, -2)}}}}, nil
}

// Generated protobuf transport must preserve unknown enum numbers all the way
// through the production read client, authorization gate and MCP JSON response.
func TestGRPCMCPUnknownQualityFlagWithholdsAndRecovers(t *testing.T) {
	engine := &unknownFlagEngine{}
	client := dialEngine(t, engine)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	policy := &auth.Policy{Roles: map[string][]auth.Action{"analyst": {auth.ActionRiskRead}}}
	gate := agentgate.NewGate(auth.NewPolicyAuthorizer(policy), client, quiet)
	readiness := &server.Readiness{}
	readiness.Set(true)
	handler := server.New(readiness, gate, client, quiet)
	call := func(t *testing.T, tenant string) (measureread.Set, string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_risk_measures","arguments":{"portfolio_id":"PF-1"}}}`))
		auth.SetPrincipalHeaders(request.Header, "analyst:user", tenant, []string{"analyst"})
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var response struct {
			Error  json.RawMessage `json:"error"`
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if tenant != "t1" {
			if len(response.Error) == 0 || len(response.Result.Content) != 0 {
				t.Fatalf("another tenant received a governed projection: %s", recorder.Body.String())
			}
			return measureread.Set{}, recorder.Body.String()
		}
		if recorder.Code != http.StatusOK || len(response.Error) != 0 || response.Result.IsError || len(response.Result.Content) != 1 {
			t.Fatalf("authorized projection unavailable: %s", recorder.Body.String())
		}
		text := response.Result.Content[0].Text
		var set measureread.Set
		if err := json.Unmarshal([]byte(text), &set); err != nil {
			t.Fatal(err)
		}
		return set, text
	}
	for _, flag := range []int32{5, -1, 1<<31 - 1, -1 << 31} {
		t.Run(fmt.Sprint(flag), func(t *testing.T) {
			engine.flag.Store(flag)
			var first string
			for attempt := range 2 {
				set, body := call(t, "t1")
				if len(set.Measures) != 1 || len(set.QualityFlags) != 1 || set.QualityFlags[0] != fmt.Sprint(flag) {
					t.Fatalf("gRPC/MCP lost unknown flag identity: %s", body)
				}
				m := set.Measures[0]
				if m.Status != measureread.StatusUnavailable || m.Value != nil || m.UncertaintyAbs != nil || !strings.Contains(m.Reason, fmt.Sprint(flag)) || strings.Contains(body, `"value":`) || strings.Contains(body, `"uncertainty_abs":`) {
					t.Fatalf("MCP exposes a numeric claim under an unknown trust flag: %s", body)
				}
				if attempt == 0 {
					first = body
				} else if body != first {
					t.Fatal("repeated read changed the refused projection")
				}
			}
			before := engine.reads.Load()
			_, denied := call(t, "t2")
			if engine.reads.Load() != before || strings.Contains(denied, "VaR99") {
				t.Fatalf("tenant refusal leaked projection or reached a data read: %s", denied)
			}
			engine.flag.Store(int32(querypb.QualityFlag_QUALITY_FLAG_STALE))
			set, body := call(t, "t1")
			m := set.Measures[0]
			if m.Status != measureread.StatusMeasured || m.Value == nil || *m.Value != 12345 || m.UncertaintyAbs == nil || *m.UncertaintyAbs != 0.25 || len(set.QualityFlags) != 1 || set.QualityFlags[0] != "STALE" {
				t.Fatalf("known advisory response did not recover: %s", body)
			}
		})
	}
}
