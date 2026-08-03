package ledger

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"sync"
	"time"
)

// Store is the durable home of the append-only journal and the periodic book
// snapshot (PERS-01). The in-memory default below preserves exact semantics for
// tests and a single replica; a durable, replayable backend (Postgres, the
// risk-engine persist.StateStore stance) plugs in behind this interface with no
// change to the book or the fold. Append is idempotent on EntryID — the journal
// is exactly-once over an at-least-once producer.
type Store interface {
	// Append records one journal entry (idempotent on EntryID).
	Append(ctx context.Context, e *Event) error
	// Journal returns a portfolio's ENTIRE journal (the caller orders for the
	// fold). It is unbounded by construction and grows for the life of the
	// portfolio: only the snapshotter, which must fold everything to build a
	// checkpoint, and the no-checkpoint fallback in MaterializeCurrent may call
	// it. A request handler that reaches for it has reintroduced #229.
	Journal(ctx context.Context, portfolioID string) ([]*Event, error)
	// JournalSince returns the entries whose knowledge_time is strictly after
	// the given watermark — the snapshot TAIL, and the read every current-book
	// materialization takes. Note this is the mirror image of the bitemporal
	// point-in-time read (Postgres.JournalAsOf, entries at or BEFORE a bound):
	// the tail is what a checkpoint has not yet absorbed.
	JournalSince(ctx context.Context, portfolioID string, after time.Time) ([]*Event, error)
	// SaveSnapshot upserts a portfolio's latest book snapshot.
	SaveSnapshot(ctx context.Context, snap *Snapshot) error
	// LoadSnapshot returns a portfolio's latest snapshot, or ErrNoSnapshot.
	LoadSnapshot(ctx context.Context, portfolioID string) (*Snapshot, error)
	// StalePortfolios returns up to limit portfolio ids whose journal holds
	// entries past their snapshot watermark — including those with no snapshot
	// at all. It is the Snapshotter's work queue.
	StalePortfolios(ctx context.Context, limit int) ([]string, error)
}

// ErrNoSnapshot is returned by LoadSnapshot when a portfolio has no snapshot yet.
var ErrNoSnapshot = errors.New("ledger: no snapshot")

// Snapshot is a point-in-time materialization of a book plus the knowledge
// watermark it folds entries through — the PERS-01 checkpoint that bounds replay
// to the journal tail. A bitemporal as-of read (ReplayAsOf) bypasses the snapshot
// and folds the journal directly; the snapshot only accelerates the
// current-knowledge book.
type Snapshot struct {
	PortfolioID string
	Positions   map[string]*Position
	Cash        map[string]*big.Rat
	Accrued     map[string]*big.Rat
	// Through is the knowledge watermark: every journal entry with Knowledge at or
	// before Through is already folded into this snapshot.
	Through time.Time
	// MaxEffective is the latest effective_time folded into this snapshot — the
	// fence that makes resuming from it equal to a full replay.
	//
	// The fold is ORDER-SENSITIVE. Weighted-average cost realizes P&L in
	// sequence, and the canonical order is (effective, knowledge, entry_id).
	// A checkpoint is ordered by KNOWLEDGE, so a tail entry backdated before
	// this fence — a late-reported fill, a restated corporate action — would be
	// folded last when a full replay would have folded it in the middle. The
	// resulting positions, and therefore the NAV, differ.
	//
	// MaterializeCurrent compares this against the tail and REFUSES the
	// checkpoint when the tail is backdated, paying for a full replay instead.
	// A zero value means "unrecorded" and is likewise never trusted: an
	// unbounded read that is loud and slow beats a bounded read that is wrong.
	MaxEffective time.Time
}

// Snapshot captures the book's current state with the given knowledge watermark.
// The returned snapshot is a deep copy — mutating the book afterwards does not
// corrupt it.
func (b *Book) Snapshot(through time.Time) *Snapshot {
	s := &Snapshot{
		PortfolioID:  b.PortfolioID,
		Positions:    make(map[string]*Position, len(b.Positions)),
		Cash:         make(map[string]*big.Rat, len(b.Cash)),
		Accrued:      make(map[string]*big.Rat, len(b.Accrued)),
		Through:      through,
		MaxEffective: b.maxEffective,
	}
	for k, p := range b.Positions {
		s.Positions[k] = &Position{
			Qty:      new(big.Rat).Set(p.Qty),
			AvgCost:  new(big.Rat).Set(p.AvgCost),
			Realized: new(big.Rat).Set(p.Realized),
		}
	}
	for k, v := range b.Cash {
		s.Cash[k] = new(big.Rat).Set(v)
	}
	for k, v := range b.Accrued {
		s.Accrued[k] = new(big.Rat).Set(v)
	}
	return s
}

// RestoreBook rebuilds a book from a snapshot. The dedup set starts empty, so a
// subsequent fold of the journal TAIL (entries after the watermark) is safe; do
// not re-apply entries already folded into the snapshot.
func RestoreBook(s *Snapshot) *Book {
	b := NewBook(s.PortfolioID)
	b.maxEffective = s.MaxEffective
	for k, p := range s.Positions {
		b.Positions[k] = &Position{
			Qty:      new(big.Rat).Set(p.Qty),
			AvgCost:  new(big.Rat).Set(p.AvgCost),
			Realized: new(big.Rat).Set(p.Realized),
		}
	}
	for k, v := range s.Cash {
		b.Cash[k] = new(big.Rat).Set(v)
	}
	for k, v := range s.Accrued {
		b.Accrued[k] = new(big.Rat).Set(v)
	}
	return b
}

// FullScanReason says why a materialization could not be served from a
// checkpoint and read the whole journal instead. Empty means it was bounded.
//
// It is returned rather than logged because "nothing configured" and "checked,
// and fine" must never look the same: the pre-#229 code took the unbounded path
// on every single request and said nothing, which is precisely why a snapshot
// subsystem that was never wired went unnoticed from the day it was written.
type FullScanReason string

const (
	// FullScanNoSnapshot: the portfolio has no checkpoint yet. Expected for a
	// new portfolio; SUSTAINED means the Snapshotter is not running or not
	// keeping up.
	FullScanNoSnapshot FullScanReason = "no_snapshot"
	// FullScanBackdatedTail: a checkpoint exists but the tail contains an entry
	// effective BEFORE the checkpoint's fence, so resuming from it would not
	// equal a replay. See Snapshot.MaxEffective.
	FullScanBackdatedTail FullScanReason = "backdated_tail"
	// FullScanUnfencedSnapshot: the checkpoint records no MaxEffective, so the
	// backdating fence cannot be evaluated and the checkpoint is not trusted.
	FullScanUnfencedSnapshot FullScanReason = "unfenced_snapshot"
)

// MaterializeCurrent returns the current-knowledge book from the store.
//
// IT LOADS THE CHECKPOINT FIRST, ON PURPOSE (#229). Until then it called
// Journal — the entire lifetime journal, no bounds, no LIMIT — and only THEN
// loaded the snapshot, so the checkpoint bounded the in-memory fold and never
// the query. Every NAV request selected every entry the portfolio had ever had
// and allocated three *big.Rat per row while holding one pool connection. The
// comment that used to sit here claimed it was "bounded by the snapshot"; it
// was not, and could not have been, because SaveSnapshot had no caller in the
// tree and ledger_snapshots was a permanently empty table.
//
// The second return value names why the bounded path was not taken, so a caller
// can count it. An empty reason means the read was served from a checkpoint.
func MaterializeCurrent(ctx context.Context, st Store, portfolioID string) (*Book, FullScanReason, error) {
	snap, err := st.LoadSnapshot(ctx, portfolioID)
	if err != nil && !errors.Is(err, ErrNoSnapshot) {
		return nil, "", err
	}
	if err != nil {
		b, ferr := replayAll(ctx, st, portfolioID)
		return b, FullScanNoSnapshot, ferr
	}
	if snap.MaxEffective.IsZero() {
		b, ferr := replayAll(ctx, st, portfolioID)
		return b, FullScanUnfencedSnapshot, ferr
	}

	// The bounded read: only what the checkpoint has not absorbed.
	tail, err := st.JournalSince(ctx, portfolioID, snap.Through)
	if err != nil {
		return nil, "", err
	}
	for _, e := range tail {
		if e.Effective.Before(snap.MaxEffective) {
			// A backdated entry cannot be folded onto a checkpoint (see
			// Snapshot.MaxEffective). Pay for the replay; the Snapshotter's
			// next pass rebuilds the fence and the next read is bounded again.
			b, ferr := replayAll(ctx, st, portfolioID)
			return b, FullScanBackdatedTail, ferr
		}
	}

	b := RestoreBook(snap)
	for _, e := range sortedFor(tail, time.Time{}, time.Time{}, false) {
		b.Apply(e)
	}
	return b, "", nil
}

// replayAll is the unbounded fallback: the whole journal, folded from empty.
// Correct at any journal size and unusable at a large one — every caller
// records a FullScanReason so that cost is visible rather than assumed away.
func replayAll(ctx context.Context, st Store, portfolioID string) (*Book, error) {
	events, err := st.Journal(ctx, portfolioID)
	if err != nil {
		return nil, err
	}
	return Replay(portfolioID, events), nil
}

// MemoryStore is the in-process Store. Goroutine-safe.
type MemoryStore struct {
	mu        sync.RWMutex
	journal   map[string][]*Event // portfolio -> entries
	seen      map[string]bool     // entry dedup across the journal
	snapshots map[string]*Snapshot
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		journal:   make(map[string][]*Event),
		seen:      make(map[string]bool),
		snapshots: make(map[string]*Snapshot),
	}
}

func (m *MemoryStore) Append(_ context.Context, e *Event) error {
	if e == nil || e.EntryID == "" {
		return errors.New("ledger: cannot append entry with empty entry_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen[e.EntryID] {
		return nil // idempotent
	}
	m.seen[e.EntryID] = true
	m.journal[e.PortfolioID] = append(m.journal[e.PortfolioID], e)
	return nil
}

func (m *MemoryStore) Journal(_ context.Context, portfolioID string) ([]*Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.journal[portfolioID]
	out := make([]*Event, len(src))
	copy(out, src)
	return out, nil
}

// JournalSince returns the portfolio's entries known strictly after `after`.
// The in-memory store filters where Postgres pushes a range scan down; the
// RESULT must be identical, because this is the seam every DB-free test asserts
// the fold through.
func (m *MemoryStore) JournalSince(_ context.Context, portfolioID string, after time.Time) ([]*Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Event
	for _, e := range m.journal[portfolioID] {
		if e.Knowledge.After(after) {
			out = append(out, e)
		}
	}
	return out, nil
}

// StalePortfolios returns portfolios whose journal has moved past their
// snapshot watermark. Sorted, so a repeated call under a limit is stable rather
// than starving whichever portfolio the map iteration happened to skip.
func (m *MemoryStore) StalePortfolios(_ context.Context, limit int) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for id, entries := range m.journal {
		snap, ok := m.snapshots[id]
		for _, e := range entries {
			if !ok || e.Knowledge.After(snap.Through) {
				out = append(out, id)
				break
			}
		}
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) SaveSnapshot(_ context.Context, snap *Snapshot) error {
	if snap == nil || snap.PortfolioID == "" {
		return errors.New("ledger: cannot save snapshot with empty portfolio_id")
	}
	if snap.MaxEffective.IsZero() {
		// A checkpoint without its backdating fence is one MaterializeCurrent
		// will never trust, so storing it buys nothing and hides the mistake
		// behind a permanent, silent full scan. Refuse it at the seam instead.
		return errors.New("ledger: cannot save snapshot with no MaxEffective fence")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Monotonic watermark, matching the Postgres upsert's WHERE clause — see
	// there for why moving it backwards is worse than declining the write.
	if cur, ok := m.snapshots[snap.PortfolioID]; ok && snap.Through.Before(cur.Through) {
		return nil
	}
	m.snapshots[snap.PortfolioID] = snap
	return nil
}

func (m *MemoryStore) LoadSnapshot(_ context.Context, portfolioID string) (*Snapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	snap, ok := m.snapshots[portfolioID]
	if !ok {
		return nil, ErrNoSnapshot
	}
	return snap, nil
}
