// Package linkstore is the durable home of the AUDIT-01 hash-chain links the
// regulatory/sustainability ChainSigner produces (REG-02). It realizes the
// signer.LinkSink seam behind a Store interface, in the PARITY-02 stance: an
// in-memory default preserves exact semantics for tests and a single replica,
// and a Postgres backend (postgres.go) makes the chain survive a restart.
//
// # Why durability matters here
//
// signer.ChainSigner folds each filing's signature into the previous one, so a
// signature is a position in a tamper-evident chain — but only if the chain
// persists. With links held in memory only, a restart resets the head to
// Genesis and the pre-restart links vanish: the chain is no longer continuous
// or verifiable across the restart boundary, defeating the point. Persisting
// each link (and recovering the head on startup) keeps one unbroken,
// walk-verifiable chain across the deployment's lifetime.
package linkstore

import (
	"context"
	"errors"
	"sync"

	"github.com/kanz-eng/kanz/internal/audit/signer"
)

// Store durably records the hash-chain links a ChainSigner emits and recovers
// the chain head so a restart continues the chain instead of restarting at
// Genesis. Append is idempotent on the link hash — a redelivered identical
// filing signed at the same head produces the same link, recorded once.
type Store interface {
	// Append records one chain link, idempotent on its Hash (link.Cur).
	Append(ctx context.Context, link signer.Link) error
	// Head returns the hash of the most recently appended link, or "" when the
	// chain is empty (a fresh deployment starts at Genesis).
	Head(ctx context.Context) (string, error)
	// Links returns every link in chain (append) order, so a verifier can walk
	// them with chain.Verify to confirm the persisted chain is unbroken.
	Links(ctx context.Context) ([]signer.Link, error)
}

// MemoryStore is the in-process Store — the test seam and the single-replica
// default. Goroutine-safe.
type MemoryStore struct {
	mu    sync.RWMutex
	links []signer.Link
	seen  map[string]bool // link hash -> present, for idempotent append
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{seen: make(map[string]bool)}
}

// Append records the link unless its hash was already seen (idempotent).
func (m *MemoryStore) Append(_ context.Context, link signer.Link) error {
	if link.Cur == "" {
		return errors.New("linkstore: cannot append a link with an empty hash")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen[link.Cur] {
		return nil
	}
	m.seen[link.Cur] = true
	m.links = append(m.links, link)
	return nil
}

// Head returns the last appended link's hash, or "" when empty.
func (m *MemoryStore) Head(_ context.Context) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.links) == 0 {
		return "", nil
	}
	return m.links[len(m.links)-1].Cur, nil
}

// Links returns a copy of the chain in append order.
func (m *MemoryStore) Links(_ context.Context) ([]signer.Link, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]signer.Link, len(m.links))
	copy(out, m.links)
	return out, nil
}

var _ Store = (*MemoryStore)(nil)
