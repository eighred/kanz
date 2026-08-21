// Package signer is the AUDIT-01 hash-chain report signer (PARITY-04e). It
// realizes the internal/filing `Signer` seam (`Sign(canonical []byte) (string,
// error)`) with a real audit-chain link instead of the default bare SHA-256
// content hash: each signature folds in the previous signature (chain.Next), so
// a report's signature is a position in the tamper-evident audit chain and is
// verifiable by walking that chain — not just a standalone digest that proves
// nothing about ordering or completeness.
//
// There is one seam to satisfy since #633: regulatory.Signer and
// sustainability.Signer are both aliases of filing.Signer, so the composition
// root injects this into REG-01 and CLIMATE-01 report building alike.
// The signer owns the chaining math + head advancement; the durable append of
// each link (to the AUDIT-01 store / a platform.audit FACT) is a sink seam wired
// at the composition root, keeping this package free of the store's transport.
package signer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/eighred/kanz/internal/audit/chain"
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

// Chainer extends the durable chain ATOMICALLY: it reads the head, computes the
// next link, and records it, serialized against every other writer. Bound to
// linkstore.Store at the composition root.
//
// This exists because an in-process head is only single-writer-safe. Two pods each
// holding their own head both chain off the same value and FORK the chain into two
// divergent histories of signature links. With a Chainer the DATABASE owns the
// head, so N pods extend one linear chain — which is what lets regulatory scale
// past a single replica.
type Chainer interface {
	AppendChained(ctx context.Context, body []byte) (Link, error)
}

// ChainSigner signs report canonical bytes into the audit hash chain.
//
// Two modes, and the difference is who owns the chain head:
//
//   - WithChainer (production): the DATABASE owns it. Every Sign is an atomic,
//     serialized read-head→append. Safe for any number of processes.
//   - in-process (tests, the no-database default): this struct owns it under a
//     mutex. Safe within ONE process, and only one.
type ChainSigner struct {
	mu      sync.Mutex
	head    string
	sink    LinkSink
	chainer Chainer
	onErr   func(error)
	timeout time.Duration
}

// Option customizes a ChainSigner.
type Option func(*ChainSigner)

// WithSink sets the durable link appender (the AUDIT-01 store at the composition
// root). Single-writer only — the chain head stays in this process. Prefer
// WithChainer wherever more than one replica can exist.
func WithSink(sink LinkSink) Option { return func(s *ChainSigner) { s.sink = sink } }

// WithChainer makes the STORE the chain authority: each Sign atomically reads the
// head and appends, serialized across every writer. This is what makes a
// multi-replica signer safe. It supersedes WithSink when both are set.
func WithChainer(c Chainer) Option { return func(s *ChainSigner) { s.chainer = c } }

// WithErrorHandler sets a hook invoked when a durable append fails. It is for
// ALERTING ONLY — the failure is also returned from Sign, and the report is NOT
// signed. See Sign.
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

// Sign folds canonical into the audit chain and returns the resulting hash — the
// report's Signature, and its position in the chain.
//
// IT RETURNS AN ERROR IF THE LINK DID NOT DURABLY LAND, AND NO SIGNATURE.
//
// It used to log the failure and hand back the signature anyway ("the signature is
// still returned and the head still advances"). That meant a regulatory filing
// could be issued carrying a chain position that exists in no durable chain: the
// chain has a hole, and the filing claims a place in it. A filing that cannot be
// recorded must not be signed — degrade loudly, never fabricate.
func (s *ChainSigner) Sign(canonical []byte) (string, error) {
	// Copy so a later mutation of the caller's slice cannot retroactively change
	// what the chain committed to.
	body := append([]byte(nil), canonical...)

	// Production: the database owns the head, so concurrent writers — in this pod
	// or another — extend ONE linear chain.
	if s.chainer != nil {
		// The append is an audit record that must survive a cancelled request, so
		// it gets its own deadline rather than the caller's context.
		ctx, cancel := context.WithTimeout(context.Background(), s.appendTimeout())
		defer cancel()
		link, err := s.chainer.AppendChained(ctx, body)
		if err != nil {
			s.reportErr(err)
			return "", fmt.Errorf("signer: chain link not recorded, refusing to sign: %w", err)
		}
		return link.Cur, nil
	}

	// In-process chain: one writer only.
	s.mu.Lock()
	defer s.mu.Unlock()
	h := chain.Next(s.head, body)
	link := Link{Prev: s.head, Cur: h, Body: body}
	if s.sink != nil {
		if err := s.sink(link); err != nil {
			// The head does NOT advance and no signature is returned. A chain with a
			// hole is not a chain.
			s.reportErr(err)
			return "", fmt.Errorf("signer: chain link not recorded, refusing to sign: %w", err)
		}
	}
	s.head = h
	return h, nil
}

func (s *ChainSigner) appendTimeout() time.Duration {
	if s.timeout > 0 {
		return s.timeout
	}
	return 5 * time.Second
}

func (s *ChainSigner) reportErr(err error) {
	if s.onErr != nil {
		s.onErr(err)
	}
}

// Head returns the current chain head — the hash the next Sign will chain from.
func (s *ChainSigner) Head() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.head
}
