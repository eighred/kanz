package app

import (
	"context"

	domainpb "github.com/kanz-eng/kanz-schemas-go/domain/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/internal/risk/ingest"
	"github.com/kanz-eng/kanz/services/risk-engine/internal/shard"
)

// ShardFilter decorates an ingest.Applier so a replica only applies (and
// therefore only recomputes) the portfolios its consistent-hash shard owns
// (PARITY-05a). It is the OUTERMOST decorator — wrapping the
// TriggeringApplier(Store) — so an event for an unowned portfolio is dropped
// before it reaches the store OR the recompute trigger: the fan-out that
// pins each portfolio's RISK-05 per-aggregate state to a single replica.
//
// # Consumer-group pairing
//
// The gate is only correct when every replica sees every state event, so the
// owner among them can apply and the rest can drop. The composition root
// therefore subscribes each sharded replica under its OWN durable consumer
// group (a broadcast fan-out) rather than the shared, partition-balanced one
// — see cmd/risk-engine. Without that, a partition delivered to a non-owner
// would be dropped by everyone.
//
// A missing/empty aggregate id is passed through to the inner Applier, which
// owns the ErrMissingAggregateID contract — the filter never masks a
// validation error as an ownership drop.
type ShardFilter struct {
	inner  ingest.Applier
	assign *shard.Assignment
}

// NewShardFilter wraps inner with the ownership gate. A nil assignment (or one
// with an empty ring) owns everything, so wrapping is a transparent pass-
// through — callers can wire it unconditionally.
func NewShardFilter(inner ingest.Applier, assign *shard.Assignment) *ShardFilter {
	return &ShardFilter{inner: inner, assign: assign}
}

func (f *ShardFilter) owns(id string) bool {
	return id == "" || f.assign == nil || f.assign.Owns(id)
}

func (f *ShardFilter) ApplyPortfolioRevalued(ctx context.Context, env *envelopepb.Envelope, p *domainpb.PortfolioState) error {
	if !f.owns(p.PortfolioId) {
		return nil
	}
	return f.inner.ApplyPortfolioRevalued(ctx, env, p)
}

func (f *ShardFilter) ApplyPositionChanged(ctx context.Context, env *envelopepb.Envelope, p *domainpb.PositionState) error {
	if !f.owns(p.PortfolioId) {
		return nil
	}
	return f.inner.ApplyPositionChanged(ctx, env, p)
}

func (f *ShardFilter) ApplyPortfolioSnapshot(ctx context.Context, env *envelopepb.Envelope, p *domainpb.PortfolioSnapshot) error {
	if p.Portfolio != nil && !f.owns(p.Portfolio.PortfolioId) {
		return nil
	}
	return f.inner.ApplyPortfolioSnapshot(ctx, env, p)
}

// Compile-time assertion that the decorator still satisfies the contract.
var _ ingest.Applier = (*ShardFilter)(nil)
