// Package fund is the alternatives service's durable home for the commitment
// journal (ALT-01b): the append-only stream of lifecycle events the
// internal/alternatives package folds into a point-in-time fund position. It is
// the PERS-01 snapshot/replay stance applied to private-markets positions — the
// same shape as the accounting (IBOR) ledger store. The in-memory default
// preserves exact semantics for tests and a single replica; a durable backend
// plugs in behind the Store interface with no change to the fold.
package fund

import (
	"context"
	"errors"
	"sync"

	"github.com/eighred/kanz/internal/alternatives"
)

// Store is the durable journal of commitment lifecycle events. Append is
// idempotent on EventID — the journal is exactly-once over an at-least-once
// producer.
type Store interface {
	// Append records one lifecycle event (idempotent on EventID).
	Append(ctx context.Context, e *alternatives.Event) error
	// Journal returns a commitment's events (the caller folds them via
	// alternatives.Replay).
	Journal(ctx context.Context, commitmentID string) ([]*alternatives.Event, error)
	// Commitments returns the known commitment ids, for inspection/bootstrap.
	Commitments(ctx context.Context) ([]string, error)
}

// MemoryStore is the in-process Store. Goroutine-safe.
type MemoryStore struct {
	mu      sync.RWMutex
	journal map[string][]*alternatives.Event
	seen    map[string]bool
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{journal: make(map[string][]*alternatives.Event), seen: make(map[string]bool)}
}

// Append records an event, deduping on EventID.
func (m *MemoryStore) Append(_ context.Context, e *alternatives.Event) error {
	if e == nil || e.EventID == "" {
		return errors.New("fund: cannot append event with empty event_id")
	}
	if e.CommitmentID == "" {
		return errors.New("fund: cannot append event with empty commitment_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen[e.EventID] {
		return nil // idempotent
	}
	m.seen[e.EventID] = true
	m.journal[e.CommitmentID] = append(m.journal[e.CommitmentID], e)
	return nil
}

// Journal returns a copy of a commitment's events.
func (m *MemoryStore) Journal(_ context.Context, commitmentID string) ([]*alternatives.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.journal[commitmentID]
	out := make([]*alternatives.Event, len(src))
	copy(out, src)
	return out, nil
}

// Commitments returns the known commitment ids.
func (m *MemoryStore) Commitments(_ context.Context) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.journal))
	for id := range m.journal {
		out = append(out, id)
	}
	return out, nil
}

// Materialize folds a commitment's journal into its current position.
func Materialize(ctx context.Context, st Store, commitmentID string) (*alternatives.Position, error) {
	events, err := st.Journal(ctx, commitmentID)
	if err != nil {
		return nil, err
	}
	return alternatives.Replay(commitmentID, events), nil
}
