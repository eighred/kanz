package grpcsrv_test

import (
	"context"
	"net"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	risk "github.com/eighred/kanz/internal/risk"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/services/risk_engine/internal/grpcsrv"
)

func TestPositionQueryUsesActualEngineAndCanonicalWire(t *testing.T) {
	store := state.NewStore()
	stamp := time.Now().UTC().Add(-time.Second)
	p := domain.NewPortfolio("P", "USD")
	p.SetAggregate(domain.AggregateUpdate{AsOf: stamp})
	p.SetPosition(domain.Position{InstrumentID: "I-A", Quantity: &commonpb.Decimal{Coefficient: -9007199254740993, Exponent: -11}, AsOf: stamp})
	p.SetPosition(domain.Position{InstrumentID: "I-B", AsOf: stamp})
	if err := store.Restore(p, nil); err != nil {
		t.Fatal(err)
	}
	actual := engine.New(store, compute.DefaultRegistry(), risk.NewCache(), risk.NewDetector())
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	grpcsrv.New(actual, "t1").Register(grpcServer)
	done := make(chan struct{})
	go func() { defer close(done); _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() { grpcServer.Stop(); <-done })
	conn, err := grpc.NewClient("passthrough:///positions", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := querypb.NewPositionQueryServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := client.Positions(ctx, &querypb.PositionsRequest{PortfolioId: "P", PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOwnerTenant() != "t1" || resp.GetPortfolioId() != "P" || len(resp.GetPositions()) != 1 || resp.GetPositions()[0].GetQuantity().GetValue().GetCoefficient() != -9007199254740993 || resp.GetPositions()[0].GetQuantity().GetValue().GetExponent() != -11 {
		t.Fatalf("actual engine/wire changed the observation: %v", resp)
	}
	last, err := client.Positions(ctx, &querypb.PositionsRequest{PortfolioId: "P", PageSize: 1, PageToken: resp.GetNextPageToken()})
	if err != nil {
		t.Fatal(err)
	}
	if len(last.GetPositions()) != 1 || last.GetPositions()[0].GetQuantity().GetKnowledge() != querypb.StateReadKnowledge_STATE_READ_KNOWLEDGE_UNKNOWN || last.GetPositions()[0].GetQuantity().GetValue() != nil || last.GetSnapshotId() != resp.GetSnapshotId() {
		t.Fatalf("missing quantity became zero over protobuf: %v", last)
	}
	if _, err := client.Positions(ctx, &querypb.PositionsRequest{PortfolioId: "P", PageSize: 101}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unbounded page mapping: %v", err)
	}
}
