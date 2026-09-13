// Package book is the wealth service's home for household composition (WEALTH-01b):
// the accounts + holdings the internal/wealth package aggregates into a virtual
// portfolio for the household-level exposure/risk view. The in-memory default
// preserves exact semantics for tests and a single replica; a durable backend
// (and the bus consumer that folds live account/holding state) plugs in behind
// the Store interface with no change to the aggregation.
package book

import (
	"context"

	"sync"

	"github.com/eighred/kanz/internal/wealth"
)

// Store holds household composition. Put is idempotent on HouseholdID (last write
// wins) — the household is a current-state projection, not an event journal.
type Store interface {
	// Put records (or replaces) a household's composition.
	Put(ctx context.Context, h wealth.Household) error
	// Get returns a household by id; ok=false if unknown.
	Get(ctx context.Context, householdID string) (wealth.Household, bool, error)
}

// MemoryStore is the in-process Store. Goroutine-safe.
type MemoryStore struct {
	mu         sync.RWMutex
	households map[string]wealth.Household
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{households: make(map[string]wealth.Household)}
}

// Put records a household, keyed on its id.
func (m *MemoryStore) Put(_ context.Context, h wealth.Household) error {
	if err := h.ValidateValuation(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.households[h.HouseholdID] = h.Clone()
	return nil
}

// Get returns a household by id.
func (m *MemoryStore) Get(_ context.Context, householdID string) (wealth.Household, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h, ok := m.households[householdID]
	return h.Clone(), ok, nil
}
