package engine_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/state"
)

func observedPositions(t *testing.T, n int) (*engine.EngineImpl, *state.Store, time.Time) {
	t.Helper()
	e, s := newEngine()
	stamp := time.Now().UTC().Add(-time.Second)
	p := domain.NewPortfolio("P", "USD")
	p.SetAggregate(domain.AggregateUpdate{AsOf: stamp})
	for i := range n {
		p.SetPosition(domain.Position{InstrumentID: v1.InstrumentID(fmt.Sprintf("I%04d", i)), Quantity: &commonpb.Decimal{Coefficient: 9007199254740993 + int64(i), Exponent: -11}, AsOf: stamp})
	}
	if err := s.Restore(p, nil); err != nil {
		t.Fatal(err)
	}
	return e, s, stamp
}

func TestPositionsPreserveExactObservedQuantityAndBoundedPages(t *testing.T) {
	e, _, _ := observedPositions(t, 3)
	first, err := e.Positions(context.Background(), v1.PositionsRequest{PortfolioID: "P", PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Positions) != 2 || first.TotalObserved != 3 || first.NextPageToken == "" {
		t.Fatalf("bad first page: %+v", first)
	}
	if got := first.Positions[0].Quantity; got.GetCoefficient() != 9007199254740993 || got.GetExponent() != -11 {
		t.Fatalf("quantity lost precision: %v", got)
	}
	last, err := e.Positions(context.Background(), v1.PositionsRequest{PortfolioID: "P", PageSize: 2, PageToken: first.NextPageToken})
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Positions) != 1 || last.Positions[0].InstrumentID != "I0002" || last.SnapshotID != first.SnapshotID || last.NextPageToken != "" {
		t.Fatalf("bad continuation: %+v", last)
	}
	first.Positions[0].Quantity.Coefficient = 1
	again, err := e.Positions(context.Background(), v1.PositionsRequest{PortfolioID: "P"})
	if err != nil || again.Positions[0].Quantity.GetCoefficient() != 9007199254740993 {
		t.Fatalf("returned quantity escaped state ownership: %v %v", again, err)
	}
}

func TestPositionsRefuseMixedSnapshotPagination(t *testing.T) {
	e, s, stamp := observedPositions(t, 3)
	page, err := e.Positions(context.Background(), v1.PositionsRequest{PortfolioID: "P", PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	err = s.ApplyPositionChanged(context.Background(), &envelopepb.Envelope{EventId: "change", IdempotencyKey: "change"}, &domainpb.PositionState{
		PortfolioId: "P", InstrumentId: "I0001", Quantity: &commonpb.Decimal{Coefficient: -7, Exponent: -9}, AsOf: timestamppb.New(stamp.Add(time.Millisecond)),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Positions(context.Background(), v1.PositionsRequest{PortfolioID: "P", PageSize: 1, PageToken: page.NextPageToken})
	if !errors.Is(err, v1.ErrPositionSnapshotChanged) {
		t.Fatalf("mixed state accepted: %v", err)
	}
}

func TestPositionsRefuseMalformedAndOversizedSnapshots(t *testing.T) {
	e, _, _ := observedPositions(t, 1)
	for _, req := range []v1.PositionsRequest{{}, {PortfolioID: "P", PageSize: 101}, {PortfolioID: "P", PageToken: "!"}, {PortfolioID: "P", PageToken: strings.Repeat("a", 129)}} {
		if _, err := e.Positions(context.Background(), req); !errors.Is(err, v1.ErrInvalidRequest) {
			t.Fatalf("request %+v accepted: %v", req, err)
		}
	}
	bounded, _, _ := observedPositions(t, 1025)
	if _, err := bounded.Positions(context.Background(), v1.PositionsRequest{PortfolioID: "P"}); !errors.Is(err, v1.ErrPositionSnapshotBound) {
		t.Fatalf("oversized snapshot served a prefix: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Positions(ctx, v1.PositionsRequest{PortfolioID: "P"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
}

func TestPositionsPreserveMissingQuantityAsUnknown(t *testing.T) {
	e, s := newEngine()
	p := domain.NewPortfolio("P", "USD")
	p.SetPosition(domain.Position{InstrumentID: "MISSING"})
	p.SetPosition(domain.Position{InstrumentID: "MALFORMED", Quantity: &commonpb.Decimal{Coefficient: 1, Exponent: math.MaxInt32}})
	if err := s.Restore(p, nil); err != nil {
		t.Fatal(err)
	}
	resp, err := e.Positions(context.Background(), v1.PositionsRequest{PortfolioID: "P"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Positions) != 2 || len(resp.QualityFlags) == 0 {
		t.Fatalf("unknown state lost: %+v", resp)
	}
	for _, pos := range resp.Positions {
		if pos.Quantity != nil {
			t.Fatalf("unknown quantity became a number: %+v", pos)
		}
	}
}
