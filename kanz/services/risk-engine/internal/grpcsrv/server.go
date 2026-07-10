// Package grpcsrv is the risk-engine's gRPC query server (API-01b): a thin
// adapter that implements query.v1.RiskQueryService over the in-process
// risk Engine (kanz/internal/risk/api/v1.Engine, the concrete EngineImpl
// from ORCH-01b). Reads are served from the engine's view of durable state
// (PERS-01) — the server holds NO state of its own.
//
// # Adapter, not logic
//
// Every method does the same three steps: decode the proto request into the
// api/v1 request type, call the Engine, and encode the api/v1 response back
// to proto. The risk decisions (live-vs-cache fallback, staleness flags,
// scenario evaluation) all live in the Engine; this layer only translates
// and maps errors to gRPC status codes. The wire payloads
// (domain.v1.ExposureSet / RiskMeasureSet) are produced by the SAME
// publish.ToProto* converters the bus publisher uses, so a value read here
// is byte-identical to the same value seen as a FACT on the bus.
//
// # Boundary
//
// grpcsrv is part of the risk-engine service, the risk module's designated
// composition root (the RISK-02 arch test's riskComposerPrefix carve-out),
// so it may import the risk impl packages (domain, publish) the api/v*
// surface deliberately hides from other consumers.
package grpcsrv

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	querypb "github.com/kanz-eng/kanz-schemas-go/query/v1"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/internal/risk/publish"
)

// Server adapts the api/v1.Engine to the generated RiskQueryService gRPC
// interface. Safe for concurrent use — it only forwards to the Engine, which
// is itself concurrency-safe.
type Server struct {
	querypb.UnimplementedRiskQueryServiceServer
	engine v1.Engine
	// ownerTenant is the tenant that owns every portfolio this engine serves.
	// The risk-engine is single-tenant per deployment (MT-01d: one cfg.Tenant,
	// Postgres RLS scoped to it), so a portfolio visible here is owned by exactly
	// this tenant. It is stamped on the WIRE-02a owner_tenant response field — the
	// deny-by-default authz-gate input a governed client checks the caller against.
	ownerTenant string
}

// New returns a Server over the given Engine, stamping ownerTenant as the owning
// tenant of every served portfolio (WIRE-02a). Empty ownerTenant leaves the
// owner_tenant response field blank, which a deny-by-default gate treats as a
// denial — so an unconfigured tenant fails closed, never open.
func New(engine v1.Engine, ownerTenant string) *Server {
	return &Server{engine: engine, ownerTenant: ownerTenant}
}

// Register binds the server onto a grpc.ServiceRegistrar (the *grpc.Server
// the service wires up).
func (s *Server) Register(r grpc.ServiceRegistrar) {
	querypb.RegisterRiskQueryServiceServer(r, s)
}

func (s *Server) Exposure(ctx context.Context, req *querypb.ExposureRequest) (*querypb.ExposureResponse, error) {
	resp, err := s.engine.Exposure(ctx, v1.ExposureRequest{
		PortfolioID: v1.PortfolioID(req.GetPortfolioId()),
		AsOf:        asOfTime(req.GetAsOf()),
	})
	if err != nil {
		return nil, mapError(err)
	}
	es, err := protoExposureSet(resp.Set)
	if err != nil {
		return nil, err
	}
	return &querypb.ExposureResponse{
		PortfolioId:  string(resp.PortfolioID),
		AsOf:         nonZeroTimestamp(resp.AsOf),
		Set:          es,
		QualityFlags: protoFlags(resp.QualityFlags),
		OwnerTenant:  s.ownerTenant,
		// SourcePosition carries the durable-log coordinate the served state was
		// folded to (WIRE-03), the citation seed a governed reader anchors an
		// answer on. The engine leaves it nil on a degraded cache read or before
		// any positioned snapshot is applied — an absent, not a zero, coordinate.
		SourcePosition: resp.SourcePosition,
	}, nil
}

func (s *Server) Measures(ctx context.Context, req *querypb.MeasuresRequest) (*querypb.MeasuresResponse, error) {
	resp, err := s.engine.Measures(ctx, v1.MeasuresRequest{
		PortfolioID: v1.PortfolioID(req.GetPortfolioId()),
		AsOf:        asOfTime(req.GetAsOf()),
		Measures:    measureNames(req.GetMeasures()),
	})
	if err != nil {
		return nil, mapError(err)
	}
	ms, err := protoMeasureSet(resp.Set)
	if err != nil {
		return nil, err
	}
	return &querypb.MeasuresResponse{
		PortfolioId:    string(resp.PortfolioID),
		AsOf:           nonZeroTimestamp(resp.AsOf),
		Set:            ms,
		QualityFlags:   protoFlags(resp.QualityFlags),
		OwnerTenant:    s.ownerTenant,
		SourcePosition: resp.SourcePosition, // see Exposure
	}, nil
}

func (s *Server) EvaluateScenario(ctx context.Context, req *querypb.EvaluateScenarioRequest) (*querypb.EvaluateScenarioResponse, error) {
	shocks, err := apiShocks(req.GetShocks())
	if err != nil {
		return nil, err
	}
	resp, err := s.engine.EvaluateScenario(ctx, v1.ScenarioRequest{
		PortfolioID: v1.PortfolioID(req.GetPortfolioId()),
		Shocks:      shocks,
	})
	if err != nil {
		return nil, mapError(err)
	}
	ms, err := protoMeasureSet(resp.Projected)
	if err != nil {
		return nil, err
	}
	return &querypb.EvaluateScenarioResponse{
		PortfolioId:  string(resp.PortfolioID),
		Projected:    ms,
		QualityFlags: protoFlags(resp.QualityFlags),
	}, nil
}

func (s *Server) Health(ctx context.Context, _ *querypb.HealthRequest) (*querypb.HealthResponse, error) {
	h, err := s.engine.Health(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return &querypb.HealthResponse{
		Mode:      protoMode(h.Mode),
		AsOf:      nonZeroTimestamp(h.AsOf),
		Staleness: durationpb.New(h.Staleness),
	}, nil
}

// --- request decode ----------------------------------------------------

// asOfTime maps an optional request timestamp to a time.Time; a nil
// timestamp ⇒ zero time, which the Engine reads as "latest".
func asOfTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

func measureNames(in []string) []v1.MeasureName {
	if len(in) == 0 {
		return nil
	}
	out := make([]v1.MeasureName, len(in))
	for i, n := range in {
		out[i] = v1.MeasureName(n)
	}
	return out
}

// apiShocks decodes the proto shock oneof into the api/v1 concrete shock
// types. An empty/unset oneof member is an INVALID_ARGUMENT — a shock with
// no kind is a malformed request, distinct from "no shocks at all".
func apiShocks(in []*querypb.ScenarioShock) ([]v1.ScenarioShock, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]v1.ScenarioShock, 0, len(in))
	for _, sh := range in {
		switch k := sh.GetShock().(type) {
		case *querypb.ScenarioShock_Price:
			out = append(out, v1.PriceShock{
				InstrumentID: v1.InstrumentID(k.Price.GetInstrumentId()),
				Pct:          k.Price.GetPct(),
			})
		case *querypb.ScenarioShock_ParallelShift:
			out = append(out, v1.ParallelShift{Pct: k.ParallelShift.GetPct()})
		default:
			return nil, status.Error(codes.InvalidArgument, "scenario shock has no kind set")
		}
	}
	return out, nil
}

// --- response encode ---------------------------------------------------

// protoExposureSet converts the engine's ExposureSet to wire form via the
// shared publish converter. The Set is always the concrete *domain.ExposureSet
// (live or cached); anything else is an engine-internal invariant break.
func protoExposureSet(set v1.ExposureSet) (*domainpb.ExposureSet, error) {
	if set == nil {
		return nil, status.Error(codes.Internal, "engine returned nil exposure set")
	}
	es, ok := set.(*domain.ExposureSet)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected exposure set type %T", set)
	}
	return publish.ToProtoExposureSet(es), nil
}

func protoMeasureSet(set v1.MeasureSet) (*domainpb.RiskMeasureSet, error) {
	if set == nil {
		return nil, status.Error(codes.Internal, "engine returned nil measure set")
	}
	ms, ok := set.(*domain.MeasureSet)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected measure set type %T", set)
	}
	// source_event_ids are a publish-time lineage concern (the inputs that
	// produced a bus FACT); a synchronous query has no triggering event, so
	// the field is left empty here.
	return publish.ToProtoMeasureSet(ms, nil), nil
}

func protoFlags(flags []v1.QualityFlag) []querypb.QualityFlag {
	if len(flags) == 0 {
		return nil
	}
	out := make([]querypb.QualityFlag, 0, len(flags))
	for _, f := range flags {
		switch f {
		case v1.QualityFlagDegraded:
			out = append(out, querypb.QualityFlag_QUALITY_FLAG_DEGRADED)
		case v1.QualityFlagStale:
			out = append(out, querypb.QualityFlag_QUALITY_FLAG_STALE)
		}
	}
	return out
}

func protoMode(m v1.Mode) querypb.Mode {
	switch m {
	case v1.ModeNormal:
		return querypb.Mode_MODE_NORMAL
	case v1.ModeDegraded:
		return querypb.Mode_MODE_DEGRADED
	default:
		return querypb.Mode_MODE_UNSPECIFIED
	}
}

// nonZeroTimestamp maps a zero time.Time to a nil proto timestamp, so "no
// state applied yet" travels as an absent field rather than the Unix epoch.
func nonZeroTimestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// --- error mapping -----------------------------------------------------

// mapError translates the api/v1 sentinel errors + context errors to gRPC
// status codes. Unknown errors are Internal — they are bugs, not contract
// outcomes a client should special-case.
func mapError(err error) error {
	switch {
	case errors.Is(err, v1.ErrPortfolioNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, v1.ErrInvalidRequest):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// compile-time assertion the adapter satisfies the generated interface.
var _ querypb.RiskQueryServiceServer = (*Server)(nil)
