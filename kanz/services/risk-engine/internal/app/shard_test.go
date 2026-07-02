package app_test

import (
	"context"
	"fmt"
	"testing"

	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/services/risk-engine/internal/app"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/shard"
)

// recordingApplier records which portfolio ids reached the inner Applier —
// the seam that stands in for TriggeringApplier(Store): an apply that lands
// here would both mutate state AND fire a recompute.
type recordingApplier struct{ seen map[string]int }

func newRecordingApplier() *recordingApplier { return &recordingApplier{seen: map[string]int{}} }

func (r *recordingApplier) ApplyPortfolioRevalued(_ context.Context, _ *envelopepb.Envelope, p *domainpb.PortfolioState) error {
	r.seen[p.PortfolioId]++
	return nil
}
func (r *recordingApplier) ApplyPositionChanged(_ context.Context, _ *envelopepb.Envelope, p *domainpb.PositionState) error {
	r.seen[p.PortfolioId]++
	return nil
}
func (r *recordingApplier) ApplyPortfolioSnapshot(_ context.Context, _ *envelopepb.Envelope, p *domainpb.PortfolioSnapshot) error {
	if p.Portfolio != nil {
		r.seen[p.Portfolio.PortfolioId]++
	}
	return nil
}

// pickKeys returns one portfolio the replica owns and one it does not, per
// the assignment — so the assertion tracks the real ownership function
// rather than a hardcoded hash outcome.
func pickKeys(t *testing.T, a *shard.Assignment) (owned, foreign string) {
	t.Helper()
	for i := 0; i < 10000 && (owned == "" || foreign == ""); i++ {
		k := fmt.Sprintf("PORT-%d", i)
		if a.Owns(k) && owned == "" {
			owned = k
		} else if !a.Owns(k) && foreign == "" {
			foreign = k
		}
	}
	if owned == "" || foreign == "" {
		t.Fatalf("could not find both an owned and a foreign key (owned=%q foreign=%q)", owned, foreign)
	}
	return owned, foreign
}

func TestShardFilter_DropsUnownedAppliesEverySubject(t *testing.T) {
	assign := shard.NewAssignment(shard.NewRing([]string{"r0", "r1", "r2"}, 0), "r0")
	owned, foreign := pickKeys(t, assign)

	inner := newRecordingApplier()
	f := app.NewShardFilter(inner, assign)
	ctx := context.Background()
	env := &envelopepb.Envelope{}

	// Owned portfolio: all three apply methods reach the inner applier.
	_ = f.ApplyPortfolioRevalued(ctx, env, &domainpb.PortfolioState{PortfolioId: owned})
	_ = f.ApplyPositionChanged(ctx, env, &domainpb.PositionState{PortfolioId: owned, InstrumentId: "AAPL"})
	_ = f.ApplyPortfolioSnapshot(ctx, env, &domainpb.PortfolioSnapshot{Portfolio: &domainpb.PortfolioState{PortfolioId: owned}})
	if got := inner.seen[owned]; got != 3 {
		t.Fatalf("owned portfolio: inner saw %d applies, want 3", got)
	}

	// Foreign portfolio: every apply is dropped before the inner applier.
	_ = f.ApplyPortfolioRevalued(ctx, env, &domainpb.PortfolioState{PortfolioId: foreign})
	_ = f.ApplyPositionChanged(ctx, env, &domainpb.PositionState{PortfolioId: foreign, InstrumentId: "AAPL"})
	_ = f.ApplyPortfolioSnapshot(ctx, env, &domainpb.PortfolioSnapshot{Portfolio: &domainpb.PortfolioState{PortfolioId: foreign}})
	if got := inner.seen[foreign]; got != 0 {
		t.Fatalf("foreign portfolio: inner saw %d applies, want 0 (dropped)", got)
	}
}

// A nil/unsharded assignment is a transparent pass-through — the filter can
// be wired unconditionally.
func TestShardFilter_UnshardedPassesThrough(t *testing.T) {
	inner := newRecordingApplier()
	f := app.NewShardFilter(inner, shard.NewAssignment(nil, "r0"))
	_ = f.ApplyPortfolioRevalued(context.Background(), &envelopepb.Envelope{}, &domainpb.PortfolioState{PortfolioId: "ANY"})
	if inner.seen["ANY"] != 1 {
		t.Fatal("unsharded filter must pass every apply through")
	}
}

// A missing aggregate id is passed to the inner applier (which owns the
// ErrMissingAggregateID contract) — never silently dropped as unowned.
func TestShardFilter_MissingIDPassesThrough(t *testing.T) {
	inner := newRecordingApplier()
	f := app.NewShardFilter(inner, shard.NewAssignment(shard.NewRing([]string{"r0", "r1"}, 0), "r0"))
	if err := f.ApplyPositionChanged(context.Background(), &envelopepb.Envelope{}, &domainpb.PositionState{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inner.seen[""] != 1 {
		t.Fatal("empty-id apply must reach the inner applier, not be dropped")
	}
}
