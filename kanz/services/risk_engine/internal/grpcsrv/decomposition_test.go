package grpcsrv_test

import (
	"context"
	"net"
	"testing"
	"time"

	risk "github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/eighred/kanz/internal/risk/state"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	factorpb "github.com/eighred/kanz/kanz-schemas-go/factor/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"github.com/eighred/kanz/services/risk_engine/internal/grpcsrv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type vectorModel struct{ m *factormodel.Model }

func (p vectorModel) Model(context.Context, time.Time) (*factormodel.Model, bool) {
	return p.m, p.m != nil
}

func vectorClient(t *testing.T, e v1.Engine) querypb.RiskQueryServiceClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	grpcsrv.New(e, "acme").Register(srv)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
		select {
		case err := <-done:
			if err != nil && err != grpc.ErrServerStopped {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("gRPC server did not join")
		}
	})
	conn, err := grpc.NewClient("passthrough:///owned-decomposition", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return querypb.NewRiskQueryServiceClient(conn)
}

func TestDecomposeRiskActualGRPCServesRealEngineDecimalVectors(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	ts := timestamppb.New(at)
	// Unit loading and variance give B^T v = 10^9 + 10^-9 - 10^9 = 10^-9.
	// A fully covered book must retain the middle position in the public result.
	positions := []struct {
		id     string
		amount *commonpb.Decimal
	}{
		{"A", &commonpb.Decimal{Coefficient: 1, Exponent: 9}},
		{"B", &commonpb.Decimal{Coefficient: 1, Exponent: -9}},
		{"C", &commonpb.Decimal{Coefficient: -1, Exponent: 9}},
	}
	var exposures []*factorpb.FactorExposure
	for _, pos := range positions {
		exposures = append(exposures, &factorpb.FactorExposure{ModelId: "owned-wire-fixture", AsOf: ts, InstrumentId: pos.id, Loadings: []float64{1}})
	}
	m, err := factormodel.FromSnapshot(&factorpb.FactorModelSnapshot{
		Model:           &factorpb.FactorModel{ModelId: "owned-wire-fixture", AsOf: ts, Factors: []*factorpb.Factor{{Name: "MKT", Type: factorpb.FactorType_FACTOR_TYPE_STATISTICAL}}},
		Covariance:      &factorpb.FactorCovariance{ModelId: "owned-wire-fixture", AsOf: ts, Dimension: 1, Values: []float64{1}},
		Exposures:       exposures,
		InputProvenance: map[string]string{"input_digest": "owned-panel-fixture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := state.NewStore()
	if err := s.ApplyPortfolioRevalued(context.Background(), &envelopepb.Envelope{EventId: "portfolio", IdempotencyKey: "portfolio"}, &domainpb.PortfolioState{PortfolioId: "PF", BaseCurrency: "USD", AsOf: ts}); err != nil {
		t.Fatal(err)
	}
	for _, pos := range positions {
		if err := s.ApplyPositionChanged(context.Background(), &envelopepb.Envelope{EventId: pos.id, IdempotencyKey: pos.id}, &domainpb.PositionState{PortfolioId: "PF", InstrumentId: pos.id, AsOf: ts, MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: pos.amount}}); err != nil {
			t.Fatal(err)
		}
	}
	e := engine.New(s, nil, risk.NewCache(), risk.NewDetector(), engine.WithFactorModel(vectorModel{m}))
	client := vectorClient(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := client.DecomposeRisk(ctx, &querypb.DecomposeRiskRequest{PortfolioId: "PF"})
	if err != nil {
		t.Fatal(err)
	}
	residual := positions[1].amount
	if r.GetPortfolioId() != "PF" || r.GetOwnerTenant() != "acme" || r.GetCurrencyCode() != "USD" || !r.GetAsOf().AsTime().Equal(at) || !proto.Equal(r.Total, residual) || len(r.ByFactor) != 1 || !proto.Equal(r.ByFactor[0].Exposure, residual) || !proto.Equal(r.ByFactor[0].Component, residual) || len(r.ByInstrument) != len(positions) {
		t.Fatalf("actual wire decomposition: %v", r)
	}
	for i, pos := range positions {
		row := r.ByInstrument[i]
		if row.InstrumentId != pos.id || !proto.Equal(row.Marginal, &commonpb.Decimal{Coefficient: 1}) || !proto.Equal(row.Component, pos.amount) {
			t.Fatalf("wire Euler contribution for %s: %v", pos.id, row)
		}
	}
	if r.Coverage == nil || int(r.Coverage.Contributed) != len(positions) || r.Coverage.ExcludedCount != 0 || r.Provenance == nil || r.Provenance.ModelId != "owned-wire-fixture" || r.Provenance.InputDigest != "owned-panel-fixture" || !r.Provenance.ModelAsOf.AsTime().Equal(at) || r.SourcePosition != nil {
		t.Fatalf("wire coverage/provenance/unknown coordinate: %v", r)
	}
	for _, req := range []*querypb.DecomposeRiskRequest{{}, {PortfolioId: "PF", AsOf: ts}, {PortfolioId: "PF", AsOf: &timestamppb.Timestamp{Seconds: 253402300800}}} {
		if _, err := client.DecomposeRisk(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("accepted invalid/historical request %v: %v", req, err)
		}
	}
	if _, err := client.DecomposeRisk(ctx, &querypb.DecomposeRiskRequest{PortfolioId: "absent"}); status.Code(err) != codes.NotFound {
		t.Fatal(err)
	}
}

func TestDecomposeRiskGRPCPreservesFailureAndCoverageContracts(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code codes.Code
	}{
		{v1.ErrDecompositionTooLarge, codes.ResourceExhausted},
		{v1.ErrDecompositionUnavailable, codes.FailedPrecondition},
		{v1.ErrPortfolioNotOwned, codes.FailedPrecondition},
		{context.Canceled, codes.Canceled},
	} {
		srv := grpcsrv.New(fakeEngine{decompose: func(v1.DecomposeRiskRequest) (v1.DecomposeRiskResponse, error) {
			return v1.DecomposeRiskResponse{}, tc.err
		}}, "acme")
		if _, err := srv.DecomposeRisk(context.Background(), &querypb.DecomposeRiskRequest{PortfolioId: "PF"}); status.Code(err) != tc.code {
			t.Fatalf("%v became %v", tc.err, err)
		}
	}
	lp := &commonpb.LogPosition{Topic: "state", Offset: 12}
	srv := grpcsrv.New(fakeEngine{decompose: func(r v1.DecomposeRiskRequest) (v1.DecomposeRiskResponse, error) {
		return v1.DecomposeRiskResponse{PortfolioID: r.PortfolioID, SourcePosition: lp, QualityFlags: []v1.QualityFlag{v1.QualityFlagInputsUnresolved}, Decomposition: v1.RiskDecomposition{Coverage: v1.InputCoverage{ExcludedCount: 1, Exclusions: []v1.InputExclusion{{InstrumentID: "UNKNOWN", Reason: "not_in_model"}}}}}, nil
	}}, "acme")
	r, err := srv.DecomposeRisk(context.Background(), &querypb.DecomposeRiskRequest{PortfolioId: "PF"})
	if err != nil || r.Coverage == nil || r.Coverage.ExcludedCount != 1 || r.Coverage.Exclusions[0].InstrumentId != "UNKNOWN" || len(r.QualityFlags) != 1 || r.QualityFlags[0] != querypb.QualityFlag_QUALITY_FLAG_INPUTS_UNRESOLVED || !proto.Equal(r.SourcePosition, lp) {
		t.Fatalf("coverage/coordinate lost: %v %v", r, err)
	}
}
