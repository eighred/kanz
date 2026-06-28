package ledger

import (
	"context"
	"errors"
	"math/big"
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
	// Journal returns a portfolio's entries (the caller orders for the fold).
	Journal(ctx context.Context, portfolioID string) ([]*Event, error)
	// SaveSnapshot upserts a portfolio's latest book snapshot.
	SaveSnapshot(ctx context.Context, snap *Snapshot) error
	// LoadSnapshot returns a portfolio's latest snapshot, or ErrNoSnapshot.
	LoadSnapshot(ctx context.Context, portfolioID string) (*Snapshot, error)
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
}

// Snapshot captures the book's current state with the given knowledge watermark.
// The returned snapshot is a deep copy — mutating the book afterwards does not
// corrupt it.
func (b *Book) Snapshot(through time.Time) *Snapshot {
	s := &Snapshot{
		PortfolioID: b.PortfolioID,
		Positions:   make(map[string]*Position, len(b.Positions)),
		Cash:        make(map[string]*big.Rat, len(b.Cash)),
		Accrued:     make(map[string]*big.Rat, len(b.Accrued)),
		Through:     through,
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

// MaterializeCurrent returns the current-knowledge book from the store: it loads
// the latest snapshot (if any) and folds the journal tail past the watermark.
// Equivalent to Replay over the whole journal, but bounded by the snapshot.
func MaterializeCurrent(ctx context.Context, st Store, portfolioID string) (*Book, error) {
	events, err := st.Journal(ctx, portfolioID)
	if err != nil {
		return nil, err
	}
	snap, err := st.LoadSnapshot(ctx, portfolioID)
	if err != nil && !errors.Is(err, ErrNoSnapshot) {
		return nil, err
	}
	if err != nil { // ErrNoSnapshot: fold the whole journal.
		return Replay(portfolioID, events), nil
	}
	b := RestoreBook(snap)
	tail := make([]*Event, 0, len(events))
	for _, e := range events {
		if e.Knowledge.After(snap.Through) {
			tail = append(tail, e)
		}
	}
	for _, e := range sortedFor(tail, time.Time{}, time.Time{}, false) {
		b.Apply(e)
	}
	return b, nil
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

func (m *MemoryStore) SaveSnapshot(_ context.Context, snap *Snapshot) error {
	if snap == nil || snap.PortfolioID == "" {
		return errors.New("ledger: cannot save snapshot with empty portfolio_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
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
