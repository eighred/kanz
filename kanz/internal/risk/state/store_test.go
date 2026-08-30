package state_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/state"
)

var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func offset(d time.Duration) *timestamppb.Timestamp {
	return timestamppb.New(baseTime.Add(d))
}

func env(id string) *envelopepb.Envelope {
	return &envelopepb.Envelope{EventId: id, IdempotencyKey: id}
}

func money(amount int64, currency string) *commonpb.Money {
	return &commonpb.Money{
		Amount:       &commonpb.Decimal{Coefficient: amount, Exponent: 0},
		CurrencyCode: currency,
	}
}

func TestApplyPortfolioRevaluedPopulatesAggregateFields(t *testing.T) {
	s := state.NewStore()
	err := s.ApplyPortfolioRevalued(context.Background(), env("evt-1"), &domainpb.PortfolioState{
		PortfolioId:      "PORT-1",
		DisplayName:      "Alpha",
		BaseCurrency:     "USD",
		CashBalance:      money(100, "USD"),
		TotalMarketValue: money(1500, "USD"),
		PositionCount:    3,
		AsOf:             offset(0),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	p, ok := s.Snapshot("PORT-1")
	if !ok {
		t.Fatal("portfolio not stored")
	}
	if p.DisplayName() != "Alpha" {
		t.Errorf("DisplayName=%q want Alpha", p.DisplayName())
	}
	if p.BaseCurrency() != "USD" {
		t.Errorf("BaseCurrency=%q want USD", p.BaseCurrency())
	}
	if p.PositionCount() != 3 {
		t.Errorf("PositionCount=%d want 3", p.PositionCount())
	}
	if !p.AsOf().Equal(baseTime) {
		t.Errorf("AsOf=%v want %v", p.AsOf(), baseTime)
	}
}

func TestApplyPositionChangedStoresPosition(t *testing.T) {
	s := state.NewStore()
	err := s.ApplyPositionChanged(context.Background(), env("evt-1"), &domainpb.PositionState{
		PortfolioId:  "PORT-1",
		InstrumentId: "AAPL",
		Quantity:     &commonpb.Decimal{Coefficient: 100, Exponent: 0},
		AveragePrice: &commonpb.Decimal{Coefficient: 17500, Exponent: -2},
		MarketValue:  money(1800, "USD"),
		AsOf:         offset(time.Minute),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	p, _ := s.Snapshot("PORT-1")
	pos, ok := p.Position("AAPL")
	if !ok {
		t.Fatal("position not stored")
	}
	if pos.InstrumentID != "AAPL" {
		t.Errorf("InstrumentID=%q", pos.InstrumentID)
	}
	if pos.Quantity.Coefficient != 100 {
		t.Errorf("Quantity.Coefficient=%d want 100", pos.Quantity.Coefficient)
	}
}

func TestApplyMissingAggregateIDRejected(t *testing.T) {
	s := state.NewStore()
	err := s.ApplyPortfolioRevalued(context.Background(), env("evt-1"), &domainpb.PortfolioState{
		PortfolioId: "", // missing
		AsOf:        offset(0),
	})
	if err != state.ErrMissingAggregateID {
		t.Errorf("err=%v want ErrMissingAggregateID", err)
	}
	if len(s.IDs()) != 0 {
		t.Error("portfolio leaked into store despite missing id")
	}
}

func TestIdempotency_DuplicateEventIsSkipped(t *testing.T) {
	s := state.NewStore()
	ctx := context.Background()
	first := &domainpb.PortfolioState{
		PortfolioId: "PORT-1", BaseCurrency: "USD", PositionCount: 5, AsOf: offset(0),
	}
	if err := s.ApplyPortfolioRevalued(ctx, env("evt-dup"), first); err != nil {
		t.Fatal(err)
	}
	// Same idempotency_key, different payload — the second apply
	// MUST NOT mutate state.
	second := &domainpb.PortfolioState{
		PortfolioId: "PORT-1", BaseCurrency: "EUR", PositionCount: 99, AsOf: offset(time.Hour),
	}
	if err := s.ApplyPortfolioRevalued(ctx, env("evt-dup"), second); err != nil {
		t.Fatal(err)
	}
	p, _ := s.Snapshot("PORT-1")
	if p.BaseCurrency() != "USD" {
		t.Errorf("BaseCurrency=%q want USD (dedup should have blocked update)", p.BaseCurrency())
	}
	if p.PositionCount() != 5 {
		t.Errorf("PositionCount=%d want 5", p.PositionCount())
	}
}

func TestIdempotency_DistinctKeysApplyBoth(t *testing.T) {
	s := state.NewStore()
	ctx := context.Background()
	for i, key := range []string{"k1", "k2", "k3"} {
		err := s.ApplyPositionChanged(ctx, env(key), &domainpb.PositionState{
			PortfolioId:  "PORT-1",
			InstrumentId: "INST-" + key,
			Quantity:     &commonpb.Decimal{Coefficient: int64(i + 1), Exponent: 0},
			AsOf:         offset(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("apply %s: %v", key, err)
		}
	}
	p, _ := s.Snapshot("PORT-1")
	if got := len(p.Positions()); got != 3 {
		t.Errorf("positions=%d want 3", got)
	}
}

func TestSnapshot_AppliedAsHardReset(t *testing.T) {
	s := state.NewStore()
	ctx := context.Background()

	// Pre-existing position — snapshot must overwrite.
	_ = s.ApplyPositionChanged(ctx, env("pos-stale"), &domainpb.PositionState{
		PortfolioId:  "PORT-1",
		InstrumentId: "OLDSTOCK",
		Quantity:     &commonpb.Decimal{Coefficient: 1, Exponent: 0},
		AsOf:         offset(0),
	})

	snap := &domainpb.PortfolioSnapshot{
		Portfolio: &domainpb.PortfolioState{
			PortfolioId:  "PORT-1",
			DisplayName:  "Snapshotted",
			BaseCurrency: "USD",
			AsOf:         offset(time.Hour),
		},
		Positions: []*domainpb.PositionState{
			{PortfolioId: "PORT-1", InstrumentId: "AAPL", Quantity: &commonpb.Decimal{Coefficient: 50, Exponent: 0}, AsOf: offset(time.Hour)},
			{PortfolioId: "PORT-1", InstrumentId: "MSFT", Quantity: &commonpb.Decimal{Coefficient: 25, Exponent: 0}, AsOf: offset(time.Hour)},
		},
		LogPosition: &commonpb.LogPosition{Topic: "risk.portfolio.revalued", Partition: 0, Offset: 12345},
	}
	if err := s.ApplyPortfolioSnapshot(ctx, env("snap-1"), snap); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	p, _ := s.Snapshot("PORT-1")
	positions := p.Positions()
	if len(positions) != 2 {
		t.Fatalf("positions=%d want 2 (snapshot is hard reset; OLDSTOCK must be gone)", len(positions))
	}
	if _, ok := p.Position("OLDSTOCK"); ok {
		t.Error("OLDSTOCK survived snapshot reset")
	}
	if p.DisplayName() != "Snapshotted" {
		t.Errorf("DisplayName=%q", p.DisplayName())
	}
	if p.LogPosition() == nil || p.LogPosition().Offset != 12345 {
		t.Errorf("LogPosition not recorded: %v", p.LogPosition())
	}
}

func TestSnapshot_StaleSnapshotSkipped(t *testing.T) {
	s := state.NewStore()
	ctx := context.Background()

	// Apply state at T = 1h.
	_ = s.ApplyPortfolioRevalued(ctx, env("recent"), &domainpb.PortfolioState{
		PortfolioId: "PORT-1", BaseCurrency: "USD", PositionCount: 7, AsOf: offset(time.Hour),
	})
	// Older snapshot — must be silently skipped.
	snap := &domainpb.PortfolioSnapshot{
		Portfolio: &domainpb.PortfolioState{
			PortfolioId:  "PORT-1",
			BaseCurrency: "EUR",
			AsOf:         offset(0),
		},
		Positions: []*domainpb.PositionState{
			{PortfolioId: "PORT-1", InstrumentId: "STALE", Quantity: &commonpb.Decimal{Coefficient: 1, Exponent: 0}, AsOf: offset(0)},
		},
		LogPosition: &commonpb.LogPosition{Topic: "risk.portfolio.revalued", Partition: 0, Offset: 1},
	}
	if err := s.ApplyPortfolioSnapshot(ctx, env("stale-snap"), snap); err != nil {
		t.Fatal(err)
	}
	p, _ := s.Snapshot("PORT-1")
	if p.BaseCurrency() != "USD" {
		t.Errorf("BaseCurrency=%q want USD (stale snapshot must not overwrite)", p.BaseCurrency())
	}
	if _, ok := p.Position("STALE"); ok {
		t.Error("stale-snapshot position applied despite latest-wins rule")
	}
}

// -race catches data races in the two concurrent tests below.

func TestConcurrent_DifferentPortfoliosRunInParallel(t *testing.T) {
	s := state.NewStore()
	const portfolios = 20
	const eventsPer = 50
	var wg sync.WaitGroup
	for i := 0; i < portfolios; i++ {
		wg.Add(1)
		go func(pi int) {
			defer wg.Done()
			id := "PORT-" + strconv.Itoa(pi)
			ctx := context.Background()
			for j := 0; j < eventsPer; j++ {
				err := s.ApplyPositionChanged(ctx, env(id+":evt-"+strconv.Itoa(j)), &domainpb.PositionState{
					PortfolioId:  id,
					InstrumentId: "INST-" + strconv.Itoa(j),
					Quantity:     &commonpb.Decimal{Coefficient: int64(j + 1), Exponent: 0},
					AsOf:         offset(time.Duration(j) * time.Millisecond),
				})
				if err != nil {
					t.Errorf("apply: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	if got := len(s.IDs()); got != portfolios {
		t.Errorf("portfolios stored=%d want %d", got, portfolios)
	}
	for i := 0; i < portfolios; i++ {
		p, _ := s.Snapshot(v1.PortfolioID("PORT-" + strconv.Itoa(i)))
		if got := len(p.Positions()); got != eventsPer {
			t.Errorf("PORT-%d positions=%d want %d", i, got, eventsPer)
		}
	}
}

func TestConcurrent_SamePortfolioIsSerialized(t *testing.T) {
	// All goroutines target the same portfolio — the per-portfolio
	// mutex must serialize so the final position count equals the
	// number of distinct apply calls (no lost updates).
	s := state.NewStore()
	const events = 200
	var wg sync.WaitGroup
	for i := 0; i < events; i++ {
		wg.Add(1)
		go func(j int) {
			defer wg.Done()
			err := s.ApplyPositionChanged(context.Background(), env("k-"+strconv.Itoa(j)), &domainpb.PositionState{
				PortfolioId:  "PORT-1",
				InstrumentId: "INST-" + strconv.Itoa(j),
				Quantity:     &commonpb.Decimal{Coefficient: int64(j + 1), Exponent: 0},
				AsOf:         offset(time.Duration(j) * time.Millisecond),
			})
			if err != nil {
				t.Errorf("apply: %v", err)
			}
		}(i)
	}
	wg.Wait()
	p, _ := s.Snapshot("PORT-1")
	if got := len(p.Positions()); got != events {
		t.Errorf("positions=%d want %d (lost updates ⇒ per-portfolio serialization broken)", got, events)
	}
}
