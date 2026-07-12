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

	"github.com/kanz-eng/kanz/internal/audit/chain"
	"github.com/kanz-eng/kanz/internal/audit/signer"
)

// Store durably records the hash-chain links a ChainSigner emits and recovers
// the chain head so a restart continues the chain instead of restarting at
// Genesis. Append is idempotent on the link hash — a redelivered identical
// filing signed at the same head produces the same link, recorded once.
type Store interface {
	// AppendChained is the ONLY safe way to extend the chain when more than one
	// writer exists. It reads the current head, computes the next link, and
	// inserts it — ATOMICALLY, serialized against every other writer.
	//
	// The alternative (read Head(), chain in your process, then Append) is a
	// check-then-act: two pods both read the same head, both chain off it, and the
	// chain FORKS into two divergent histories of signature links. That is exactly
	// what a regulator would ask about, and it is why regulatory was pinned to one
	// replica until this existed.
	AppendChained(ctx context.Context, body []byte) (signer.Link, error)
	// Append records one pre-chained link, idempotent on its Hash (link.Cur). Safe
	// only for a single writer, or for replaying links whose chain position is
	// already decided.
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

// AppendChained chains body onto the current head under the store's lock. In one
// process this is exactly as safe as the Postgres path; across processes there is
// no shared memory, which is precisely the problem the Postgres implementation
// solves with an advisory lock.
func (m *MemoryStore) AppendChained(_ context.Context, body []byte) (signer.Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	prev := chain.Genesis
	if n := len(m.links); n > 0 {
		prev = m.links[n-1].Cur
	}
	link := signer.Link{Prev: prev, Cur: chain.Next(prev, body), Body: append([]byte(nil), body...)}
	if !m.seen[link.Cur] {
		m.links = append(m.links, link)
		m.seen[link.Cur] = true
	}
	return link, nil
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
