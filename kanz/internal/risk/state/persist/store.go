// Package persist is the durable backing store for the risk engine's
// per-portfolio state (PERS-01). The in-memory state.Store (RISK-05) is
// the hot, authoritative copy the engine computes against; this package
// is the cold copy that lets a restarted engine resume without gaps or
// double-counting.
//
// # Why durable state at all
//
// The engine is event-sourced: every state change arrives as a FACT on
// the bus and the in-memory Store rebuilds the world by applying them.
// Replaying the entire Kafka log of record on every restart is correct
// but unbounded — startup time grows with history. The
// snapshot-then-resume contract (event-class-rules §3) bounds it: a
// PortfolioSnapshot carries the LogPosition it was taken at, so on
// restart the engine loads the latest snapshot and replays the log only
// from LogPosition+1 forward. This package is where those snapshots live
// between runs.
//
// # The durable record, not the domain type
//
// StateStore trades in PortfolioRecord, a flat data-only value, rather
// than *domain.Portfolio. Same two-layer reasoning as the proto-shape vs
// domain-shape split (see domain package doc): domain.Portfolio is the
// engine's working shape (unexported fields, mutation discipline,
// Clone), PortfolioRecord is the durable shape (the exact set of fields
// SQL columns map to). FromPortfolio / ToPortfolio are the single
// canonical mapping between them so 01b's Postgres impl, 01c's snapshot
// emitter, and 01d's bootstrap path never re-derive it divergently.
//
// # RPO=0 hinges on atomicity
//
// Save persists one portfolio's aggregate fields, its full position set,
// its LogPosition, and its recently-applied idempotency keys as a single
// atomic, per-aggregate transaction (PERS-01b takes a per-portfolio row
// lock so concurrent saves serialize, mirroring the in-memory
// per-portfolio mutex). The LogPosition is committed in the same
// transaction as the state it describes — a snapshot can never claim to
// include events it didn't, which is what makes the resume gap-free.
//
// # Idempotency across restarts
//
// The in-memory dedup window (RISK-05) is lost on restart, so the events
// replayed from the snapshot's LogPosition could be applied twice at the
// boundary (a portfolio is fed from multiple topic-partitions, so one
// scalar LogPosition cannot be a perfect watermark for all of them).
// PortfolioRecord.AppliedKeys carries a bounded tail of the idempotency
// keys already folded into the snapshot; the bootstrap path rehydrates
// the in-memory dedup window from them before replay, so a replayed
// boundary event is recognized as already-applied and skipped — the same
// dedup machinery, just pre-seeded. This is why idempotency is persisted
// as part of the record rather than checked per-event against the DB:
// the hot apply path stays in memory.
package persist

import (
	"context"
	"errors"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
)

// StateStore persists and restores committed risk-engine state.
// Implementations must be safe for concurrent use across portfolios;
// concurrent Save calls for the SAME portfolio are serialized by the
// implementation (per-aggregate locking, PERS-01b).
type StateStore interface {
	// Save durably persists one portfolio's committed state — aggregate
	// fields, positions, LogPosition, and AppliedKeys — in a single
	// atomic, per-aggregate transaction. A full replace of that
	// portfolio's persisted row set (positions absent from rec.Positions
	// are removed), matching the snapshot hard-reset semantics
	// (event-class-rules §3).
	Save(ctx context.Context, rec PortfolioRecord) error

	// Load returns the latest persisted record for one portfolio, or
	// ErrNotFound when none has been saved.
	Load(ctx context.Context, id v1.PortfolioID) (PortfolioRecord, error)

	// LoadEach streams every persisted portfolio record to fn, one at a
	// time, in ascending portfolio-id order. This is the bootstrap read
	// (PERS-01d) that rehydrates engine state before the log replay
	// begins. It returns the first error fn returns, and stops.
	//
	// IT IS A CALLBACK AND NOT A SLICE (#674). The previous shape,
	// LoadAll(ctx) ([]PortfolioRecord, error), could only be implemented
	// by materialising the whole estate: peak memory and startup time
	// both scaled with total portfolio count, which makes recovery time a
	// function of how large the book has grown. Handing records over one
	// at a time is what makes a bounded implementation possible, and
	// removing the slice-returning shape is what stops a well-meaning
	// caller reintroducing the coupling.
	//
	// An implementation MUST NOT hold every record to satisfy this.
	LoadEach(ctx context.Context, fn func(PortfolioRecord) error) error

	// Ping checks store reachability for the readiness probe.
	Ping(ctx context.Context) error
}

// PortfolioRecord is the durable shape of one portfolio's committed
// state. Money / Decimal fields are the same common.v1 value types the
// domain and wire layers use; the Postgres impl stores them as marshaled
// proto bytes (exact base-10, since double is banned for money/sizes).
// LogPosition is decomposed into its own columns (topic/partition/offset)
// so operators can read resume coordinates without decoding — see the
// migration.
type PortfolioRecord struct {
	ID v1.PortfolioID
	// TenantID is the owning tenant (MT-01d). On Save it is stamped by the
	// store from the session GUC `app.tenant_id` and enforced by RLS, so a
	// record is always written under the engine's authenticated tenant; on
	// Load it is read back from the row. Empty on records built in-memory
	// before persistence (FromPortfolio) — the DB is the authority.
	TenantID         string
	DisplayName      string
	BaseCurrency     domain.CurrencyCode
	CashBalance      *commonpb.Money
	TotalMarketValue *commonpb.Money
	PositionCount    uint32
	// AsOf is the state timestamp the record was taken at — the
	// most-recent applied event time across the portfolio's aggregate
	// and positions.
	AsOf time.Time
	// LogPosition is the durable-log coordinate this record includes up
	// to; the bootstrap path resumes the log at LogPosition.Offset+1.
	// nil before any snapshot has been taken.
	LogPosition *commonpb.LogPosition
	// Positions is the full position set; domain.Position is already a
	// flat value struct (no behavior) so it doubles as the durable
	// position shape without a parallel type.
	Positions []domain.Position
	// AppliedKeys is a bounded tail of idempotency keys already folded
	// into this record, used to pre-seed the in-memory dedup window on
	// bootstrap so replayed boundary events are not double-counted.
	AppliedKeys []string
}

// FromPortfolio builds a durable record from the engine's working copy
// plus the idempotency keys to persist alongside it. The caller passes a
// clone (state.Store.Snapshot) so the read is race-free.
func FromPortfolio(p *domain.Portfolio, appliedKeys []string) PortfolioRecord {
	return PortfolioRecord{
		ID:               p.ID(),
		DisplayName:      p.DisplayName(),
		BaseCurrency:     p.BaseCurrency(),
		CashBalance:      p.CashBalance(),
		TotalMarketValue: p.TotalMarketValue(),
		PositionCount:    p.PositionCount(),
		AsOf:             p.AsOf(),
		LogPosition:      p.LogPosition(),
		Positions:        p.Positions(),
		AppliedKeys:      appliedKeys,
	}
}

// ToPortfolio rebuilds the engine's working copy from a durable record —
// the bootstrap rehydration step (PERS-01d), run before log replay.
// AppliedKeys are not folded in here; the bootstrap path reads them off
// the record to pre-seed the dedup window separately.
func (r PortfolioRecord) ToPortfolio() *domain.Portfolio {
	p := domain.NewPortfolio(r.ID, r.BaseCurrency)
	p.SetAggregate(domain.AggregateUpdate{
		AsOf:             r.AsOf,
		DisplayName:      r.DisplayName,
		BaseCurrency:     r.BaseCurrency,
		CashBalance:      r.CashBalance,
		TotalMarketValue: r.TotalMarketValue,
		PositionCount:    r.PositionCount,
	})
	for _, pos := range r.Positions {
		p.SetPosition(pos)
	}
	if r.LogPosition != nil {
		p.SetSnapshot(r.AsOf, r.LogPosition)
	}
	return p
}

// ErrNotFound is returned by Load when no record exists for the
// portfolio. The portfolio may still exist in the log of record with no
// snapshot yet taken.
var ErrNotFound = errors.New("persist: portfolio not found")
