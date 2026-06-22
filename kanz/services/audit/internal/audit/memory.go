package audit

import (
	"context"
	"sync"

	"github.com/kanz-eng/kanz/services/audit/internal/chain"
)

// Memory is an in-memory append-only audit store. It is the reference Store
// implementation (and the test/dev backend); the Postgres store mirrors its
// semantics for durability. Safe for concurrent use.
type Memory struct {
	mu      sync.RWMutex
	records []*Record // ordered by Seq (append order)
	byID    map[string]*Record
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{byID: make(map[string]*Record)}
}

func (m *Memory) Append(_ context.Context, r *Record) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.byID[r.EventID]; ok {
		return existing, nil // idempotent on EventID
	}

	prev := chain.Genesis
	if n := len(m.records); n > 0 {
		prev = m.records[n-1].HashV
	}
	stored := *r // copy — the caller's record is not mutated/retained
	stored.Seq = int64(len(m.records)) + 1
	stored.PrevHashV = prev
	stored.HashV = chain.Next(prev, stored.Canonical())

	m.records = append(m.records, &stored)
	m.byID[stored.EventID] = &stored
	return &stored, nil
}

func (m *Memory) Get(_ context.Context, eventID string) (*Record, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.byID[eventID]
	return r, ok, nil
}

func (m *Memory) Query(_ context.Context, f Filter) ([]*Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Record, 0)
	for _, r := range m.records {
		if matches(r, f) {
			out = append(out, r)
			if f.Limit > 0 && len(out) >= f.Limit {
				break
			}
		}
	}
	return out, nil
}

func (m *Memory) All(_ context.Context) ([]*Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Record, len(m.records))
	copy(out, m.records)
	return out, nil
}

func (m *Memory) Head(_ context.Context) (Head, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if n := len(m.records); n > 0 {
		return Head{Seq: m.records[n-1].Seq, Hash: m.records[n-1].HashV}, nil
	}
	return Head{Seq: 0, Hash: chain.Genesis}, nil
}

func (m *Memory) Ping(context.Context) error { return nil }

var _ Store = (*Memory)(nil)
