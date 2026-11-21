package replay

import (
	"context"
	"net"
	"slices"
	"testing"
	"time"

	risk "github.com/eighred/kanz/internal/risk"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/publish"
	"github.com/eighred/kanz/internal/risk/state"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"github.com/eighred/kanz/services/risk_engine/internal/grpcsrv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestAsOfReplayThroughGRPCWithColdLiveState(t *testing.T) {
	a, _ := testRepositories(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e, err := New(compute.DefaultRegistry(), a, "test")
	if err != nil {
		t.Fatal(err)
	}
	p := testPortfolio()
	original, err := e.Compute(ctx, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := evaluationKnowledge(t, a)
	cache := risk.NewCache()
	query := engine.New(state.NewStore(), compute.DefaultRegistry(), cache, risk.NewDetector(), engine.WithEvaluator(e))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	grpcsrv.New(query, "artifact-a").Register(server)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := querypb.NewRiskQueryServiceClient(conn)
	response, err := client.Measures(ctx, &querypb.MeasuresRequest{PortfolioId: string(p.ID()), AsOf: timestamppb.New(cutoff)})
	if err != nil {
		t.Fatal(err)
	}
	if response.OwnerTenant != "artifact-a" || !response.AsOf.AsTime().Equal(p.AsOf()) || !proto.Equal(response.Set, publish.ToProtoMeasureSet(original.Measures, nil)) {
		t.Fatal("wire response lost historical state, provenance or tenant ownership")
	}
	_, err = client.Measures(ctx, &querypb.MeasuresRequest{PortfolioId: string(p.ID()), AsOf: timestamppb.New(cutoff.Add(-time.Microsecond))})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing history returned %v", err)
	}
	_, err = client.Measures(ctx, &querypb.MeasuresRequest{PortfolioId: string(p.ID())})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("historical read contaminated current state: %v", err)
	}

	p.SetPosition(domain.Position{InstrumentID: "B", MarketValue: &commonpb.Money{CurrencyCode: "USD"}})
	if _, err := e.Compute(ctx, p, nil); err != nil {
		t.Fatal(err)
	}
	read := func(at time.Time) *querypb.ExposureResponse {
		t.Helper()
		result, err := client.Exposure(ctx, &querypb.ExposureRequest{PortfolioId: string(p.ID()), AsOf: timestamppb.New(at)})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	incomplete := read(evaluationKnowledge(t, a))
	if !slices.Contains(incomplete.QualityFlags, querypb.QualityFlag_QUALITY_FLAG_INPUTS_UNRESOLVED) {
		t.Fatalf("missing historical coverage flag: %v", incomplete.QualityFlags)
	}
	found := false
	for _, row := range incomplete.GetSet().GetExposures() {
		if row.Dimension == domainpb.ExposureDimension_EXPOSURE_DIMENSION_CURRENCY && row.Bucket == "USD" {
			found = true
			if row.Gross.GetAmount() != nil || row.Net.GetAmount() != nil {
				t.Fatal("partial historical USD total")
			}
		}
	}
	if !found {
		t.Fatal("missing historical USD bucket")
	}
	earlier := read(cutoff)
	if len(earlier.QualityFlags) != 0 || !proto.Equal(earlier.Set, publish.ToProtoExposureSet(original.Exposure)) {
		t.Fatal("later incomplete mark changed the earlier historical exposure")
	}
}
