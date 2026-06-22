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
	// All returns every record ordered by Seq — the input to chain verification
	// (AUDIT-01b) and lineage walks (AUDIT-01c).
	All(ctx context.Context) ([]*Record, error)
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
