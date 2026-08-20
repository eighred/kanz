// Package state owns the per-aggregate ordered, idempotent state
// application for the risk engine — RISK-05.
//
// # Why a separate package from ingest
//
// RISK-04's Ingestor does pure routing (bus delivery → proto type
// → Applier call). RISK-05's Store owns the ENGINE STATE: the map
// of Portfolios, the per-portfolio locks, the per-portfolio dedup
// windows. Splitting the two keeps the routing fast and stateless;
// the per-aggregate guarantees live with the state they protect.
//
// # Per-aggregate ordering primitive
//
// Each portfolio holds its own sync.Mutex inside Store.locks. Apply
// methods take the per-portfolio lock before mutating, so two events
// for the same portfolio are serialized regardless of which bus
// subscription delivered them — the bus only guarantees per-
// partition ordering (event-class-rules §1), but a portfolio
// receives events on multiple partitions (PortfolioState on one,
// PositionState on others), and per-aggregate ordering across all
// of them is THE correctness invariant for the engine. The mutex
// is the cheapest primitive that gives us this — alternatives
// (per-aggregate goroutine + channel) add complexity without
// changing the contract.
//
// # Idempotency
//
// Each portfolio has its own dedupWindow (defense-in-depth alongside
// the bus.Consumer's global window, EVT-17d). Apply methods consult
// it BEFORE mutating and Record AFTER successful apply — a transient
// apply failure must remain retryable. Per-portfolio scoping (rather
// than global) matches event-class-rules §1 dedup windows and avoids
// the global-map contention that would otherwise be the engine's
// hottest write.
//
// # Snapshot semantics
//
// PortfolioSnapshot applies are latest-wins-by-event-time per
// event-class-rules §3: a snapshot whose payload.Portfolio.AsOf is
// older than the current portfolio.AsOf is skipped silently. A
// newer (or first-ever) snapshot is a HARD RESET — positions are
// cleared, the snapshot's positions applied, and the LogPosition
// recorded so RISK-04's bootstrap path can resume the log from
// exactly that point.
//
// # Lazy portfolio creation
//
// Because the bus does not guarantee cross-event-type ordering, a
// PositionState may arrive before the corresponding PortfolioState.
// Apply methods lazy-create the Portfolio on first reference with
// an empty BaseCurrency; the first PortfolioState or
// PortfolioSnapshot fills it in. The alternative ("reject events
// for unknown portfolios") would reject many valid sequences during
// rolling deployments and bootstrap.
package state

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/ingest"
)

// Store is the risk engine's state-of-the-world. Implements
// ingest.Applier (writer side) and exposes Lookup (reader side).
// Compute layers (RISK-06/07/08) and the api/v1 surface read
// portfolios through Lookup.
type Store struct {
	// mu guards the maps themselves (lookup-or-create of locks /
	// portfolios / dedup windows). Per-portfolio mu is held briefly
	// — once the per-portfolio mutex is acquired, mu is released.
	mu         sync.Mutex
	portfolios map[v1.PortfolioID]*domain.Portfolio
	locks      map[v1.PortfolioID]*sync.Mutex
	dedup      map[v1.PortfolioID]*dedupWindow

	// loader/owned are the runtime-ownership gate (#110, see ownership.go).
	// BOTH nil is the ungated default: this replica owns every portfolio and
	// lazy-creates on first reference, which is the deployed single-replica
	// posture. Non-nil owned means only an Acquired portfolio is answerable.
	loader Loader
	owned  map[v1.PortfolioID]struct{}

	// shardOwns is the STATIC ownership source (#110, see ownership.go): the
	// consistent-hash ring's verdict, fixed for the process lifetime because
	// the member list is. Nil means "no ring", not "owns nothing".
	shardOwns func(v1.PortfolioID) bool
}

// NewStore returns an empty Store. With no options it is the ungated,
// owns-everything store the unsharded posture runs on; WithShardOwnership and
// WithRuntimeOwnership turn on the #110 gate (see ownership.go).
func NewStore(opts ...Option) *Store {
	s := &Store{
		portfolios: make(map[v1.PortfolioID]*domain.Portfolio),
		locks:      make(map[v1.PortfolioID]*sync.Mutex),
		dedup:      make(map[v1.PortfolioID]*dedupWindow),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Lookup returns the portfolio and true, or nil and false. The
// returned pointer is the engine's live copy — callers within the
// risk module must read it while holding the appropriate lock or
// clone before passing it across an apply boundary. The api/v1
// surface always clones before returning to external callers.
func (s *Store) Lookup(id v1.PortfolioID) (*domain.Portfolio, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.portfolios[id]
	return p, ok
}

// Snapshot returns a race-free deep clone of the portfolio, taken while
// holding the per-aggregate lock, or nil/false when the portfolio is
// unknown. This is the locked-read boundary the compute/query paths use
// instead of Lookup: applies for the same portfolio are serialized by
// the same lock, so the clone is a consistent point-in-time view that
// the caller can compute against while subsequent applies proceed on the
// live copy. Cheap — typical portfolios hold tens to hundreds of
// value-copied positions.
func (s *Store) Snapshot(id v1.PortfolioID) (*domain.Portfolio, bool) {
	s.mu.Lock()
	lock := s.locks[id]
	port, ok := s.portfolios[id]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	if lock != nil {
		lock.Lock()
		defer lock.Unlock()
	}
	return port.Clone(), true
}

// SnapshotWithKeys returns a race-free clone of the portfolio together
// with its applied idempotency-key tail, both captured under the same
// per-aggregate lock so the persisted (state, dedup-tail) pair is
// consistent — an apply cannot interleave to record a key whose state
// effect is missing from the clone, or vice versa. This is the read the
// PERS-01c snapshotter uses to build a durable PortfolioRecord. Returns
// nil/nil/false when the portfolio is unknown.
func (s *Store) SnapshotWithKeys(id v1.PortfolioID) (*domain.Portfolio, []string, bool) {
	s.mu.Lock()
	lock := s.locks[id]
	port, ok := s.portfolios[id]
	dw := s.dedup[id]
	s.mu.Unlock()
	if !ok {
		return nil, nil, false
	}
	if lock != nil {
		lock.Lock()
		defer lock.Unlock()
	}
	var keys []string
	if dw != nil {
		keys = dw.Keys()
	}
	return port.Clone(), keys, true
}

// IDs returns the set of known portfolio IDs. Used by the api/v1
// surface's Health implementation and by tests.
func (s *Store) IDs() []v1.PortfolioID {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]v1.PortfolioID, 0, len(s.portfolios))
	for id := range s.portfolios {
		out = append(out, id)
	}
	return out
}

// Restore installs a rehydrated portfolio and pre-seeds its dedup window
// from the persisted applied-key tail — the in-memory side of the PERS-01d
// bootstrap (load durable snapshot → Restore → replay the log). Pre-seeding
// the window is what makes the subsequent log replay idempotent: an event
// already folded into the snapshot is recognized as Seen and skipped rather
// than double-counted.
//
// Restore runs single-threaded during bootstrap, before any ingestion
// goroutine starts, so it takes only the map lock (no per-aggregate lock)
// and is a no-op if the portfolio is already present — bootstrap must not
// clobber live state, and a snapshot is never newer than the running engine.
func (s *Store) Restore(p *domain.Portfolio, appliedKeys []string) error {
	if p == nil || p.ID() == "" {
		return ErrEmptyPortfolioID
	}
	id := p.ID()
	s.mu.Lock()
	defer s.mu.Unlock()
	// ON A GATED STORE, BOOT IS NOT A BACK DOOR. Restore predates the
	// ownership gate and installs state directly; letting it through for an
	// unacquired portfolio would put a portfolio in the maps that the gate
	// says this replica must not answer for — the exact half-state Acquire
	// exists to prevent. Acquire is the only door in gated mode.
	if !s.ownsLocked(id) {
		return fmt.Errorf("%w: %q (gated store: Acquire it first)", ErrNotOwned, id)
	}
	if _, exists := s.portfolios[id]; exists {
		return nil
	}
	s.portfolios[id] = p
	if _, ok := s.locks[id]; !ok {
		s.locks[id] = &sync.Mutex{}
	}
	dw := newDedupWindow()
	for _, k := range appliedKeys {
		dw.Record(k)
	}
	s.dedup[id] = dw
	return nil
}

// ApplyPortfolioRevalued satisfies ingest.Applier.
func (s *Store) ApplyPortfolioRevalued(ctx context.Context, env *envelopepb.Envelope, p *domainpb.PortfolioState) error {
	id := v1.PortfolioID(p.PortfolioId)
	if id == "" {
		return ErrMissingAggregateID
	}
	lock, port, dw, err := s.perAggregate(id)
	if err != nil {
		return err
	}
	lock.Lock()
	defer lock.Unlock()

	if dw.Seen(env.IdempotencyKey) {
		return nil
	}
	port.SetAggregate(domain.AggregateUpdate{
		AsOf:             tsTime(p.AsOf),
		DisplayName:      p.DisplayName,
		BaseCurrency:     domain.CurrencyCode(p.BaseCurrency),
		CashBalance:      p.CashBalance,
		TotalMarketValue: p.TotalMarketValue,
		PositionCount:    p.PositionCount,
	})
	dw.Record(env.IdempotencyKey)
	return nil
}

// ApplyPositionChanged satisfies ingest.Applier.
func (s *Store) ApplyPositionChanged(ctx context.Context, env *envelopepb.Envelope, p *domainpb.PositionState) error {
	id := v1.PortfolioID(p.PortfolioId)
	if id == "" || p.InstrumentId == "" {
		return ErrMissingAggregateID
	}
	lock, port, dw, err := s.perAggregate(id)
	if err != nil {
		return err
	}
	lock.Lock()
	defer lock.Unlock()

	if dw.Seen(env.IdempotencyKey) {
		return nil
	}
	port.SetPosition(toDomainPosition(p))
	dw.Record(env.IdempotencyKey)
	return nil
}

// ApplyPortfolioSnapshot satisfies ingest.Applier. Snapshot apply is
// a hard reset (event-class-rules §3, latest-wins by event_time).
func (s *Store) ApplyPortfolioSnapshot(ctx context.Context, env *envelopepb.Envelope, p *domainpb.PortfolioSnapshot) error {
	if p.Portfolio == nil {
		return ErrMissingAggregateID
	}
	id := v1.PortfolioID(p.Portfolio.PortfolioId)
	if id == "" {
		return ErrMissingAggregateID
	}
	lock, port, dw, err := s.perAggregate(id)
	if err != nil {
		return err
	}
	lock.Lock()
	defer lock.Unlock()

	if dw.Seen(env.IdempotencyKey) {
		return nil
	}
	// Latest-wins: a snapshot older than the current state is a
	// stale broker delivery. Skip silently — recording it in dedup
	// is fine (it's a real envelope we've decided not to apply).
	snapAsOf := tsTime(p.Portfolio.AsOf)
	if !port.AsOf().IsZero() && snapAsOf.Before(port.AsOf()) {
		dw.Record(env.IdempotencyKey)
		return nil
	}

	port.ClearPositions()
	for _, ps := range p.Positions {
		port.SetPosition(toDomainPosition(ps))
	}
	port.SetAggregate(domain.AggregateUpdate{
		AsOf:             snapAsOf,
		DisplayName:      p.Portfolio.DisplayName,
		BaseCurrency:     domain.CurrencyCode(p.Portfolio.BaseCurrency),
		CashBalance:      p.Portfolio.CashBalance,
		TotalMarketValue: p.Portfolio.TotalMarketValue,
		PositionCount:    p.Portfolio.PositionCount,
	})
	port.SetSnapshot(snapAsOf, p.LogPosition)
	dw.Record(env.IdempotencyKey)
	return nil
}

// perAggregate returns the per-portfolio (lock, portfolio, dedup),
// lazy-creating each on first reference. mu is held briefly so the
// usual fast path is two map reads under one lock.
//
// THE OWNERSHIP GATE IS HERE, not in the callers, and it is inside the same mu
// critical section as the lazy-create. A check-then-create in each Apply method
// would leave a window where a concurrent Release lands between the two and the
// apply lazy-creates a portfolio this replica no longer owns — an empty book
// that reads as a real one. Ungated stores answer true and behave exactly as
// before (#110).
func (s *Store) perAggregate(id v1.PortfolioID) (*sync.Mutex, *domain.Portfolio, *dedupWindow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ownsLocked(id) {
		return nil, nil, nil, fmt.Errorf("%w: %q", ErrNotOwned, id)
	}
	lock, ok := s.locks[id]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[id] = lock
	}
	port, ok := s.portfolios[id]
	if !ok {
		// Lazy-create with empty BaseCurrency — first PortfolioState
		// or PortfolioSnapshot fills it in. See package doc.
		port = domain.NewPortfolio(id, "")
		s.portfolios[id] = port
	}
	dw, ok := s.dedup[id]
	if !ok {
		dw = newDedupWindow()
		s.dedup[id] = dw
	}
	return lock, port, dw, nil
}

// toDomainPosition is the proto→domain mapping for PositionState.
// Lives in this package (not domain) so the domain package stays
// independent of kanz-schemas/domain/v1 — only common/v1 leaks
// through Position's fields.
func toDomainPosition(p *domainpb.PositionState) domain.Position {
	return domain.Position{
		InstrumentID:  domain.InstrumentID(p.InstrumentId),
		Quantity:      p.Quantity,
		AveragePrice:  p.AveragePrice,
		MarketValue:   p.MarketValue,
		RealizedPnL:   p.RealizedPnl,
		UnrealizedPnL: p.UnrealizedPnl,
		AsOf:          tsTime(p.AsOf),
	}
}

// tsTime converts an optional protobuf Timestamp to time.Time,
// returning the Go zero time for nil. Using the concrete pointer
// type (rather than an AsTime interface) avoids the typed-nil-vs-
// interface-nil trap — calling AsTime() on a nil *Timestamp returns
// the Unix epoch, not Go's zero time, which would falsely look like
// "state applied" to AsOf checks.
func tsTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

// Sentinel errors.
var (
	// ErrMissingAggregateID is returned when an event payload omits
	// the portfolio_id (or instrument_id, for PositionState). The
	// envelope validator (bus.Validate) cannot catch this — the
	// payload schema declares the field "required" by doc convention
	// but proto3 has no presence — so the Store enforces it.
	ErrMissingAggregateID = errIngestErr("missing aggregate id in state payload")
)

type errIngestErr string

func (e errIngestErr) Error() string { return string(e) }

// Compile-time assertion that Store satisfies ingest.Applier. Same
// pattern as the api/v1 interface assertions in domain — catches
// contract drift at build time.
var _ ingest.Applier = (*Store)(nil)
