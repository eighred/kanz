package tools

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/governed"
	"github.com/eighred/kanz/services/copilot/internal/llm"
	"github.com/eighred/kanz/services/copilot/internal/retrieval"
)

// THE GATE, OVER A REAL WIRE.
//
// Every other test in this package reaches the governed surface through an
// in-process double. That is the right level for the gate's LOGIC, and it is the
// wrong level for the question #741 actually turned on: which failures the
// production client classes as "the portfolio is not visible to you"
// (ErrUnknownPortfolio, tenant-indistinguishable) and which it classes as "the
// read surface did not answer". That classification is done by mapErr against a
// gRPC status code, and a hand-made error never exercises it.
//
// So this suite runs a REAL grpc.Server on a real loopback listener, serving the
// real query.v1 service, behind the real governed.GRPCClient, the real
// PolicyAuthorizer and the real AuditedAuthorizer. The only thing it controls is
// what the server answers.
//
// It also counts the READS THE SERVER SAW. A refusal that still reached the
// engine is not a refusal, and no assertion on the returned string can tell the
// difference.

// gateServer is a query.v1 server whose Exposure answer is scripted, and which
// records every read that arrived.
type gateServer struct {
	querypb.UnimplementedRiskQueryServiceServer

	ownerTenant  string
	exposureErr  error
	mu           sync.Mutex
	exposureHits int
	measureHits  int
	scenarioHits int
}

func (s *gateServer) Exposure(_ context.Context, _ *querypb.ExposureRequest) (*querypb.ExposureResponse, error) {
	s.mu.Lock()
	s.exposureHits++
	s.mu.Unlock()
	if s.exposureErr != nil {
		return nil, s.exposureErr
	}
	return &querypb.ExposureResponse{PortfolioId: "PF-T1", OwnerTenant: s.ownerTenant}, nil
}

func (s *gateServer) Measures(_ context.Context, _ *querypb.MeasuresRequest) (*querypb.MeasuresResponse, error) {
	s.mu.Lock()
	s.measureHits++
	s.mu.Unlock()
	return &querypb.MeasuresResponse{PortfolioId: "PF-T1", OwnerTenant: s.ownerTenant}, nil
}

func (s *gateServer) EvaluateScenario(_ context.Context, _ *querypb.EvaluateScenarioRequest) (*querypb.EvaluateScenarioResponse, error) {
	s.mu.Lock()
	s.scenarioHits++
	s.mu.Unlock()
	return &querypb.EvaluateScenarioResponse{PortfolioId: "PF-T1", OwnerTenant: s.ownerTenant}, nil
}

// reads is every call that reached the engine BEYOND the ownership lookup. The
// ownership lookup itself rides on Exposure, so one Exposure hit is the gate
// doing its job; a Measures or Scenario hit is data being served.
func (s *gateServer) reads() (owner, data int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exposureHits, s.measureHits + s.scenarioHits
}

// serveGate stands up the server on a loopback listener and returns a Registry
// wired to it through the production gRPC client.
func serveGate(t *testing.T, srv *gateServer) (*Registry, *capRecorder) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	querypb.RegisterRiskQueryServiceServer(gs, srv)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = gs.Serve(lis)
	}()
	t.Cleanup(func() {
		gs.Stop()
		<-done
	})

	// Insecure on loopback: mTLS is the composition root's job (and is its own
	// contract test); what is under test here is the gate above the transport.
	cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	rec := &capRecorder{}
	quiet := slog.New(slog.NewTextHandler(discard{}, nil))
	authz := auth.NewAuditedAuthorizer(auth.NewPolicyAuthorizer(testPolicy()), rec, "copilot", quiet)
	client := governed.NewGRPCClient(querypb.NewRiskQueryServiceClient(cc))
	return NewRegistry(authz, client, retrieval.IdentityCatalog{}, quiet), rec
}

func t2Intruder() *auth.Principal {
	return &auth.Principal{Subject: "mallory", Tenant: "t2", Roles: []string{"analyst"}}
}

// A real DEADLINE_EXCEEDED from the engine must refuse the read, not remove the
// boundary. This is the production shape of #741: mapErr passes the status
// through untouched (only NOT_FOUND becomes ErrUnknownPortfolio), the owner
// resolves to "", and that used to mean "not tenant-scoped".
func TestGRPCGate_TransientStatusRefusesTheCrossTenantRead(t *testing.T) {
	srv := &gateServer{ownerTenant: "t1", exposureErr: status.Error(codes.DeadlineExceeded, "engine busy")}
	reg, rec := serveGate(t, srv)

	out := reg.Invoke(context.Background(), t2Intruder(),
		llm.ToolCall{Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}})

	if !out.IsError {
		t.Fatalf("a cross-tenant read was served over a failing ownership lookup: %s", out.Content)
	}
	if _, data := srv.reads(); data != 0 {
		t.Fatalf("the engine served %d data read(s) behind a failed ownership lookup — the tenant "+
			"boundary was decided by the engine's availability", data)
	}
	if out.Content != msgOwnerUnavailable {
		t.Errorf("content = %q, want %q", out.Content, msgOwnerUnavailable)
	}
	if len(rec.logs) == 0 || rec.logs[len(rec.logs)-1].GetAttributes()["deny.code"] != string(auth.DenyResourceTenantUnresolved) {
		t.Errorf("the attempt was not audited as an unresolved-tenant refusal: %+v", rec.logs)
	}
}

// The claim governed/grpc.go has carried since WIRE-02b — "an empty owner_tenant
// resolves to ” and the deny-by-default gate then denies, so a missing owner
// fails closed, never open" — was FALSE for as long as the gate skipped on an
// empty resource tenant. The comment described the property the code did not
// have. This is that property, tested rather than asserted.
func TestGRPCGate_EmptyOwnerTenantFromTheEngineFailsClosed(t *testing.T) {
	srv := &gateServer{ownerTenant: ""} // OK status, no ownership record
	reg, _ := serveGate(t, srv)

	// The portfolio's own tenant, with the granting role: only isolation can
	// refuse this.
	out := reg.Invoke(context.Background(), t1Analyst(),
		llm.ToolCall{Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}})

	if !out.IsError {
		t.Fatalf("an unattributed portfolio was SERVED: %s", out.Content)
	}
	if _, data := srv.reads(); data != 0 {
		t.Fatalf("the engine served %d data read(s) for a portfolio it holds no owner for", data)
	}
	if out.Content != msgNoGovernedData+"PF-T1" {
		t.Errorf("content = %q, want %q", out.Content, msgNoGovernedData+"PF-T1")
	}
}

// A real NOT_FOUND is the one status that means "not visible to you", and it
// must stay tenant-indistinguishable rather than being reported as an outage.
func TestGRPCGate_NotFoundReadsAsMissingNotAsAnOutage(t *testing.T) {
	srv := &gateServer{ownerTenant: "t1", exposureErr: status.Error(codes.NotFound, "unknown portfolio")}
	reg, _ := serveGate(t, srv)

	out := reg.Invoke(context.Background(), t1Analyst(),
		llm.ToolCall{Name: "get_exposure", Input: map[string]any{"portfolio_id": "PF-T1"}})

	if !out.IsError || out.Content != msgNoGovernedData+"PF-T1" {
		t.Fatalf("content = %q, want %q", out.Content, msgNoGovernedData+"PF-T1")
	}
	if _, data := srv.reads(); data != 0 {
		t.Fatalf("engine served %d data read(s) for a NOT_FOUND portfolio", data)
	}
}

// A genuine cross-tenant probe, with the engine answering perfectly well: the
// refusal is the not-found wording, and it names neither the owning tenant nor
// the boundary that refused it.
func TestGRPCGate_CrossTenantProbeIsAnsweredAsNotFound(t *testing.T) {
	srv := &gateServer{ownerTenant: "t1"}
	reg, rec := serveGate(t, srv)

	out := reg.Invoke(context.Background(), t2Intruder(),
		llm.ToolCall{Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}})

	if !out.IsError || out.Content != msgNoGovernedData+"PF-T1" {
		t.Fatalf("content = %q, want %q", out.Content, msgNoGovernedData+"PF-T1")
	}
	if _, data := srv.reads(); data != 0 {
		t.Fatalf("engine served %d data read(s) to another tenant", data)
	}
	for _, leak := range []string{"t1", "tenant", "cross"} {
		if strings.Contains(out.Content, leak) {
			t.Errorf("refusal %q carries %q", out.Content, leak)
		}
	}
	// The audit trail keeps what the caller was denied.
	last := rec.logs[len(rec.logs)-1]
	if last.GetAttributes()["deny.code"] != string(auth.DenyCrossTenant) {
		t.Errorf("deny.code = %q, want %q", last.GetAttributes()["deny.code"], auth.DenyCrossTenant)
	}
}

// NON-VACUITY FOR THE WHOLE FILE: the same wiring, with the engine healthy and
// the caller entitled, must SERVE. Without this row every assertion above is
// satisfied by a registry that refuses everything, and a gate that refuses
// everything proves nothing about a gate.
func TestGRPCGate_EntitledCallerIsServed(t *testing.T) {
	srv := &gateServer{ownerTenant: "t1"}
	reg, _ := serveGate(t, srv)

	out := reg.Invoke(context.Background(), t1Analyst(),
		llm.ToolCall{Name: "get_risk_measures", Input: map[string]any{"portfolio_id": "PF-T1"}})

	if out.IsError {
		t.Fatalf("the owning tenant was refused its own portfolio: %s", out.Content)
	}
	owner, data := srv.reads()
	if owner == 0 {
		t.Error("the ownership lookup never reached the engine — the gate is not consulting it")
	}
	if data != 1 {
		t.Fatalf("engine saw %d data read(s), want exactly 1", data)
	}
}
