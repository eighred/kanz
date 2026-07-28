// Package store is the data master's durable home for the resolved golden
// records and the price-exception oversight queue (PARITY-02b). It defines the
// seams the server composes over — an in-memory default (the test seam,
// preserving the exact MASTER-01 contract) and a Postgres backend (postgres.go)
// that survives a restart with no change to resolution or arbitration.
package store

import (
	"context"
	"math/big"
	"sync"
	"time"

	"github.com/eighred/kanz/services/datamaster/internal/master"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
)

// GoldenStore persists resolved golden records — replace-on-write on the
// canonical instrument id (a resolution is the whole truth for an instrument).
type GoldenStore interface {
	Put(ctx context.Context, rec master.SecurityMaster) error
	Get(ctx context.Context, instrumentID string) (master.SecurityMaster, bool, error)
}

// ExceptionStore persists the oversight exception queue with the exact
// in-memory contract: Add is idempotent on the deterministic exception id (a
// re-detected break does not wipe its review state), and Override appends an
// immutable audit record, moving the entry to OVERRIDDEN.
type ExceptionStore interface {
	Add(ctx context.Context, e pricing.Exception) error
	AddAll(ctx context.Context, exs []pricing.Exception) error
	Override(ctx context.Context, id, actor, reason string, chosenPrice *big.Rat, at time.Time) error
	Get(ctx context.Context, id string) (pricing.Exception, bool, error)
	Open(ctx context.Context) ([]pricing.Exception, error)
}

// MemoryGoldenStore is the in-process GoldenStore. Goroutine-safe.
type MemoryGoldenStore struct {
	mu      sync.RWMutex
	records map[string]master.SecurityMaster
}

// NewMemoryGoldenStore returns an empty in-memory GoldenStore.
func NewMemoryGoldenStore() *MemoryGoldenStore {
	return &MemoryGoldenStore{records: make(map[string]master.SecurityMaster)}
}

func (m *MemoryGoldenStore) Put(_ context.Context, rec master.SecurityMaster) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[rec.InstrumentID] = rec
	return nil
}

func (m *MemoryGoldenStore) Get(_ context.Context, instrumentID string) (master.SecurityMaster, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.records[instrumentID]
	return rec, ok, nil
}

// QueueStore adapts the in-memory pricing.Queue to the ExceptionStore seam, so
// the existing idempotent-add / append-only-override logic is reused verbatim
// as the test default.
type QueueStore struct {
	q *pricing.Queue
}

// NewQueueStore wraps a pricing.Queue (or a fresh one if nil).
func NewQueueStore(q *pricing.Queue) *QueueStore {
	if q == nil {
		q = pricing.NewQueue()
	}
	return &QueueStore{q: q}
}

func (s *QueueStore) Add(_ context.Context, e pricing.Exception) error { s.q.Add(e); return nil }

func (s *QueueStore) AddAll(_ context.Context, exs []pricing.Exception) error {
	s.q.AddAll(exs)
	return nil
}

func (s *QueueStore) Override(_ context.Context, id, actor, reason string, chosenPrice *big.Rat, at time.Time) error {
	return s.q.Override(id, actor, reason, chosenPrice, at)
}

func (s *QueueStore) Get(_ context.Context, id string) (pricing.Exception, bool, error) {
	e, ok := s.q.Get(id)
	return e, ok, nil
}

func (s *QueueStore) Open(_ context.Context) ([]pricing.Exception, error) { return s.q.Open(), nil }

var (
	_ GoldenStore    = (*MemoryGoldenStore)(nil)
	_ ExceptionStore = (*QueueStore)(nil)
)
