// Package signer is the AUDIT-01 hash-chain report signer (PARITY-04e). It
// realizes the regulatory/sustainability `Signer` seam (`Sign(canonical []byte)
// string`) with a real audit-chain link instead of the default bare SHA-256
// content hash: each signature folds in the previous signature (chain.Next), so
// a report's signature is a position in the tamper-evident audit chain and is
// verifiable by walking that chain — not just a standalone digest that proves
// nothing about ordering or completeness.
//
// One value satisfies both internal/regulatory.Signer and
// internal/sustainability.Signer implicitly (same one-method interface), so the
// composition root injects it into REG-01 and CLIMATE-01 report building alike.
// The signer owns the chaining math + head advancement; the durable append of
// each link (to the AUDIT-01 store / a platform.audit FACT) is a sink seam wired
// at the composition root, keeping this package free of the store's transport.
package signer

import (
	"sync"

	"github.com/kanz-eng/kanz/services/audit/internal/chain"
)

// Link is one signed report's view as the audit chain sees it: the predecessor
// hash it chained from, its own hash (== the report Signature), and the exact
// canonical bytes that were signed. It satisfies chain.Link, so a verifier walks
// a slice of these with chain.Verify.
type Link struct {
	Prev string
	Cur  string
	Body []byte
}

func (l Link) PrevHash() string  { return l.Prev }
func (l Link) Hash() string      { return l.Cur }
func (l Link) Canonical() []byte { return l.Body }
func (l Link) Signature() string { return l.Cur }

var _ chain.Link = Link{}

// LinkSink durably appends one chain link. The composition root wires it to the
// AUDIT-01 store (or a platform.audit FACT producer); nil ⇒ links are chained
// in memory only (still verifiable within the process). Called under the
// signer's lock, in chain order, so appends are serialized.
type LinkSink func(link Link) error

// ChainSigner signs report canonical bytes into the audit hash chain. Safe for
// concurrent Sign calls; each advances the shared head atomically so the chain
// stays linear even when REG-01 and CLIMATE-01 reports sign concurrently.
type ChainSigner struct {
	mu    sync.Mutex
	head  string
	sink  LinkSink
	onErr func(error)
}

// Option customizes a ChainSigner.
type Option func(*ChainSigner)

// WithSink sets the durable link appender (the AUDIT-01 store at the composition
// root).
func WithSink(sink LinkSink) Option { return func(s *ChainSigner) { s.sink = sink } }

// WithErrorHandler sets the hook invoked when the sink returns an error. The
// signature is still returned and the head still advances (the in-memory chain
// stays consistent); the hook lets a deployment alert on a failed durable append.
func WithErrorHandler(fn func(error)) Option { return func(s *ChainSigner) { s.onErr = fn } }

// New returns a ChainSigner whose chain starts at head — pass the current audit
// head to continue an existing chain, or "" to start from chain.Genesis.
func New(head string, opts ...Option) *ChainSigner {
	if head == "" {
		head = chain.Genesis
	}
	s := &ChainSigner{head: head}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Sign folds canonical into the chain (H(head || canonical)), advances the head,
// appends the link via the sink, and returns the new hash — the report's
// Signature, and its position in the audit chain.
func (s *ChainSigner) Sign(canonical []byte) string {
	// Copy so a later mutation of the caller's slice can't retroactively change
	// what the chain committed to.
	body := append([]byte(nil), canonical...)
	s.mu.Lock()
	defer s.mu.Unlock()
	h := chain.Next(s.head, body)
	link := Link{Prev: s.head, Cur: h, Body: body}
	if s.sink != nil {
		if err := s.sink(link); err != nil && s.onErr != nil {
			s.onErr(err)
		}
	}
	s.head = h
	return h
}

// Head returns the current chain head — the hash the next Sign will chain from.
func (s *ChainSigner) Head() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.head
}
