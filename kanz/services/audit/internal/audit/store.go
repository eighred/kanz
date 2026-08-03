package audit

import (
	"context"
	"time"
)

// Head is the current tip of the audit hash chain — the last record's sequence
// and hash, which the next append chains from.
type Head struct {
	Seq  int64
	Hash string
}

// Filter selects records for Query. Zero-value fields are ignored, so an empty
// Filter matches everything (bounded by Limit). Times are inclusive.
type Filter struct {
	Correlation string
	Tenant      string
	Kind        Kind
	EventType   string
	Since       time.Time
	Until       time.Time
	Limit       int
}

// Store is the append-only audit log. Implementations MUST be append-only — no
// update, no delete (that is the WORM contract AUDIT-01b's tamper-evidence rests
// on). Append assigns Seq + the chain hashes and is idempotent on EventID, so a
// redelivered event does not duplicate or fork the chain.
type Store interface {
	// Append records r (filling Seq, PrevHash, Hash) and returns the stored
	// record. If a record with r.EventID already exists, it returns the existing
	// record and does not append (idempotent projection).
	Append(ctx context.Context, r *Record) (*Record, error)
	// Get returns the record for eventID, if present.
	Get(ctx context.Context, eventID string) (*Record, bool, error)
	// Query returns records matching f, ordered by Seq ascending.
	Query(ctx context.Context, f Filter) ([]*Record, error)
	// Scan STREAMS every record ordered by Seq to yield — the input to chain
	// verification (AUDIT-01b). It stops and returns yield's error at the first
	// non-nil one.
	//
	// THE RECORD IS ONLY VALID FOR THE DURATION OF THE CALL. yield must fold it
	// and let it go; a caller that keeps the pointer must copy first. This is
	// what makes the streaming guarantee enforceable rather than aspirational —
	// an implementation is free to reuse one buffer, so a caller that
	// accumulates gets a deliberate, immediate failure instead of quietly
	// reintroducing the whole-log slice this method exists to remove. (The
	// shipped implementations do allocate per row; the contract is the point.)
	//
	// It replaces an All() that returned every record as a slice (#229).
	// Verification is inherently whole-chain — a suffix cannot be verified
	// without a trusted anchor for what precedes it — so unlike the accounting
	// ledger this read cannot be BOUNDED. What it must not do is materialise the
	// entire compliance log in memory to walk it once: the chain walk is a left
	// fold, so the caller's working set is one record regardless of log size.
	// Streaming is the half of the problem that is soluble here.
	//
	// The read still holds one pool connection for the whole scan and its
	// duration still grows with the log. Fixing THAT needs periodically signed
	// chain checkpoints to anchor a partial verification, which is a security
	// design (who signs, where the anchor lives, what stops it being rewritten
	// alongside the log) and not a refactor.
	Scan(ctx context.Context, yield func(*Record) error) error
	// Head returns the current chain tip (Genesis-derived for an empty log).
	Head(ctx context.Context) (Head, error)
	// Ping checks store liveness for readiness.
	Ping(ctx context.Context) error
}

// matches reports whether r satisfies f (shared by the in-memory store and
// tests; the Postgres store pushes the same predicates into SQL).
func matches(r *Record, f Filter) bool {
	if f.Correlation != "" && r.CorrelationID != f.Correlation {
		return false
	}
	if f.Tenant != "" && r.TenantID != f.Tenant {
		return false
	}
	if f.Kind != "" && r.Kind != f.Kind {
		return false
	}
	if f.EventType != "" && r.EventType != f.EventType {
		return false
	}
	if !f.Since.IsZero() && r.OccurredAt.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && r.OccurredAt.After(f.Until) {
		return false
	}
	return true
}
