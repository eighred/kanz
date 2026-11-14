package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/agent"
	"github.com/eighred/kanz/services/copilot/internal/governed"
	"github.com/eighred/kanz/services/copilot/internal/llm"
	"github.com/eighred/kanz/services/copilot/internal/retrieval"
	"github.com/eighred/kanz/services/copilot/internal/server"
	"github.com/eighred/kanz/services/copilot/internal/tools"
)

type groundingQuery struct {
	querypb.UnimplementedRiskQueryServiceServer
	coefficient   int64
	owners, reads atomic.Int32
}

func (q *groundingQuery) Exposure(context.Context, *querypb.ExposureRequest) (*querypb.ExposureResponse, error) {
	q.owners.Add(1)
	return &querypb.ExposureResponse{PortfolioId: "PF-T1", OwnerTenant: "t1"}, nil
}

func (q *groundingQuery) Measures(context.Context, *querypb.MeasuresRequest) (*querypb.MeasuresResponse, error) {
	q.reads.Add(1)
	return &querypb.MeasuresResponse{
		PortfolioId: "PF-T1", OwnerTenant: "t1",
		Set:            &domainpb.RiskMeasureSet{Measures: []*domainpb.RiskMeasure{{Name: "Delta", Value: &commonpb.Decimal{Coefficient: q.coefficient, Exponent: -2}}}},
		AsOf:           timestamppb.New(time.Date(2026, 11, 14, 12, 0, 0, 123, time.UTC)),
		SourcePosition: &commonpb.LogPosition{Topic: "risk.position", Partition: 2, Offset: 43},
	}, nil
}

// Capture AFTER the real recorder succeeds. The socket-backed test compares this
// Agent-built record with both live delivery and a new retained-log subscription.
type groundingRecorder struct {
	delegate agent.AnswerRecorder
	record   *observationpb.AgentAnswerRecord
}

func (r *groundingRecorder) RecordAnswer(ctx context.Context, rec *observationpb.AgentAnswerRecord) error {
	if r.delegate != nil {
		if err := r.delegate.RecordAnswer(ctx, rec); err != nil {
			return err
		}
	}
	r.record = proto.Clone(rec).(*observationpb.AgentAnswerRecord)
	return nil
}

type groundingCase struct {
	name, answer string
	coefficient  int64
	unsupported  []string
}

func groundingCases() []groundingCase {
	return []groundingCase{
		{"signed", "Delta is -0.42.", 42, []string{"-0.42"}},
		{"scientific", "Delta is 1e+9.", 42, []string{"1e+9"}},
		{"percentage_overflow", "Delta is 1e308.", 42, []string{"1e308"}},
		{"backed_signed", "Delta is -0.42.", -42, nil},
		{"backed_scientific", "Delta is 4.2e-1.", 42, nil},
	}
}

func groundingDigest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:16])
}

// This drives the public HTTP boundary, actual Agent/tool registry/authorizer,
// generated gRPC client+server and shared measure projection. Only the model and
// the risk service's returned state are dummy fixtures; no provider is contacted.
func askGroundingNumber(t *testing.T, tc groundingCase, principal, tenant string, delegate agent.AnswerRecorder) *observationpb.AgentAnswerRecord {
	t.Helper()
	q := &groundingQuery{coefficient: tc.coefficient}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	querypb.RegisterRiskQueryServiceServer(gs, q)
	done := make(chan struct{})
	go func() { defer close(done); _ = gs.Serve(lis) }()
	t.Cleanup(func() { gs.Stop(); _ = lis.Close(); <-done })
	cc, err := grpc.NewClient("passthrough:///grounding", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	log := quietLogger()
	authz := auth.NewAuditedAuthorizer(auth.NewPolicyAuthorizer(&auth.Policy{Roles: map[string][]auth.Action{"analyst": {auth.ActionRiskRead}}}), auth.NewSlogRecorder(log), "copilot", log)
	registry := tools.NewRegistry(authz, governed.NewGRPCClient(querypb.NewRiskQueryServiceClient(cc)), retrieval.IdentityCatalog{}, log)
	model := llm.NewStubModel(
		llm.Response{Model: "fixture/tool", ToolCalls: []llm.ToolCall{{ID: "read", Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}}}},
		llm.Response{Model: "fixture/answer", Text: tc.answer, StopReason: llm.StopEndTurn},
	)
	rec := &groundingRecorder{delegate: delegate}
	mx := newAnswerMetrics()
	metrics := prometheus.NewPedanticRegistry()
	mx.register(metrics)
	var observations []agent.Observation
	a := agent.New(model, registry, agent.WithRecorder(rec), agent.WithAnswerObserver(func(o agent.Observation) { observations = append(observations, o); mx.observe(o) }))
	r := &server.Readiness{}
	r.Set(true)
	s := server.New(r, nil, a)
	const question = "What is the portfolio Delta?"
	req := httptest.NewRequest(http.MethodPost, "/v1/ask", strings.NewReader(`{"question":"`+question+`"}`)).WithContext(t.Context())
	auth.SetPrincipalHeaders(req.Header, principal, tenant, []string{"analyst"})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ask: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Answer     string
		Grounded   bool
		Ungrounded []string
		Citations  []string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	wantGrounded := len(tc.unsupported) == 0
	if body.Answer != tc.answer || body.Grounded != wantGrounded || !reflect.DeepEqual(body.Ungrounded, append([]string{}, tc.unsupported...)) {
		t.Fatalf("HTTP verdict = %+v, want grounded %v unsupported %v", body, wantGrounded, tc.unsupported)
	}
	if q.owners.Load() != 1 {
		t.Fatalf("ownership lookups=%d", q.owners.Load())
	}
	if rec.record == nil {
		t.Fatal("Agent answer never reached recorder")
	}
	got := rec.record
	wantReads, wantCites := int32(1), 1
	if tenant != "t1" {
		wantReads, wantCites = 0, 0
	}
	if q.reads.Load() != wantReads || len(body.Citations) != wantCites || len(got.GetRetrieved()) != wantCites {
		t.Fatalf("authorized projection: reads=%d citations=%v retrieved=%v", q.reads.Load(), body.Citations, got.GetRetrieved())
	}
	if wantCites == 1 {
		ref := got.GetRetrieved()[0]
		if ref.GetPortfolioId() != "PF-T1" || ref.GetSourceEventId() != "risk.position@2:43" || ref.GetLineageNode() != "risk.position@2:43" || ref.GetAsOf() != "2026-11-14T12:00:00Z" || got.GetContextDigest() == "" {
			t.Fatalf("record lost governed source lineage: %v", got)
		}
	} else if got.GetContextDigest() != "" {
		t.Fatalf("cross-tenant context recorded: %v", got)
	}
	if got.GetGrounded() != wantGrounded || !reflect.DeepEqual(got.GetUngroundedClaims(), tc.unsupported) || got.GetTenantId() != tenant || got.GetPrincipalSubject() != principal || got.GetOutcome() != observationpb.AgentAnswerOutcome_AGENT_ANSWER_OUTCOME_ANSWERED || !reflect.DeepEqual(got.GetModels(), []string{"fixture/tool", "fixture/answer"}) || got.GetQuestionDigest() != groundingDigest(question) || got.GetAnswerDigest() != groundingDigest(tc.answer) {
		t.Fatalf("Agent record disagrees with HTTP/identity/transcript: %v", got)
	}
	if len(observations) != 1 || observations[0].Grounded != wantGrounded || observations[0].UngroundedClaims != len(tc.unsupported) || !observations[0].RecordKept || observations[0].Measured != wantCites {
		t.Fatalf("missing/incorrect answer observation: %+v", observations)
	}
	wantCount := float64(0)
	if !wantGrounded {
		wantCount = 1
	}
	if got := seriesIn(t, metrics, "kanz_copilot_ungrounded_answers_total")[""]; got != wantCount {
		t.Fatalf("ungrounded answer counter=%v want %v", got, wantCount)
	}
	if got := seriesIn(t, metrics, "kanz_copilot_ungrounded_claims_total")[""]; got != float64(len(tc.unsupported)) {
		t.Fatalf("ungrounded claim counter=%v", got)
	}
	return got
}

func TestGroundingNumbersReachHTTPAndObservation(t *testing.T) {
	for _, tc := range groundingCases() {
		t.Run(tc.name, func(t *testing.T) { askGroundingNumber(t, tc, "grounding-analyst", "t1", nil) })
	}
	t.Run("cross_tenant_refuses_projection", func(t *testing.T) {
		askGroundingNumber(t, groundingCase{"", "Delta is -0.42.", 42, []string{"-0.42"}}, "other-analyst", "t2", nil)
	})
}
