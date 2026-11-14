package engine_test

import (
	"context"
	"testing"
	"time"

	risk "github.com/eighred/kanz/internal/risk"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/state"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestMalformedStateTimeDoesNotTriggerRecompute(t *testing.T) {
	for _, form := range []string{"portfolio", "position", "snapshot-portfolio", "snapshot-position"} {
		t.Run(form, func(t *testing.T) {
			store := state.NewStore()
			applyPosition(t, store, "PF", "OLD", 500, time.Now())
			cache := risk.NewCache()
			r := engine.NewRecomputer(context.Background(), store, compute.DefaultRegistry(), cache, nil, time.Hour, discard())
			defer r.Close()
			a := engine.NewTriggeringApplier(store, r)
			env := &envelopepb.Envelope{EventId: "bad", IdempotencyKey: "bad"}
			bad := &timestamppb.Timestamp{Nanos: -1}
			p := &domainpb.PortfolioState{PortfolioId: "PF", BaseCurrency: "USD", AsOf: timestamppb.Now()}
			pos := &domainpb.PositionState{PortfolioId: "PF", InstrumentId: "NEW", MarketValue: money(900, "USD"), AsOf: timestamppb.Now()}
			var err error
			switch form {
			case "portfolio":
				p.AsOf = bad
				err = a.ApplyPortfolioRevalued(context.Background(), env, p)
			case "position":
				pos.AsOf = bad
				err = a.ApplyPositionChanged(context.Background(), env, pos)
			case "snapshot-portfolio":
				p.AsOf = bad
				err = a.ApplyPortfolioSnapshot(context.Background(), env, &domainpb.PortfolioSnapshot{Portfolio: p, Positions: []*domainpb.PositionState{pos}})
			case "snapshot-position":
				pos.AsOf = bad
				err = a.ApplyPortfolioSnapshot(context.Background(), env, &domainpb.PortfolioSnapshot{Portfolio: p, Positions: []*domainpb.PositionState{pos}})
			}
			if err == nil {
				t.Fatal("malformed state apply succeeded")
			}
			// Drain forces any wrongly queued trigger through compute; no timing
			// sleep or empty-book shortcut can make the negative assertion pass.
			r.Drain()
			if _, ok := cache.LookupExposure("PF"); ok {
				t.Fatal("rejected input scheduled an exposure recompute")
			}
			if _, ok := cache.LookupMeasures("PF"); ok {
				t.Fatal("rejected input scheduled measures")
			}
		})
	}
}
