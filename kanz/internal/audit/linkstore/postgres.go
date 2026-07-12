package linkstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanz-eng/kanz/internal/audit/chain"
	"github.com/kanz-eng/kanz/internal/audit/signer"
)

// Postgres is the durable Store backed by the 0001_audit_links.sql schema — a
// single append-only, monotonically-ordered sequence of chain links (REG-02).
// It reuses the risk-engine persist.Postgres / accounting ledger.Postgres
// stance (pgxpool, caller-owned lifecycle). The audit chain is a single
// per-deployment sequence, not tenant-owned — the regulatory service is a
// stateless filing assembler with no principal — so the table carries no
// tenant_id and no RLS, matching the RISK-12 universal-fact store rationale.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a Postgres store over an existing pool. The caller owns
// the pool lifecycle (Close), matching the other durable stores.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Ping verifies connectivity so startup fails fast on a bad DSN rather than on
// the first filing sign.
func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// chainLockKey namespaces the advisory lock that serializes chain extension. Any
// constant works; it only has to be the same in every process that appends.
const chainLockKey int64 = 0x6b616e7a4348414e // "kanzCHAN"

// AppendChained extends the chain ATOMICALLY: it reads the head, computes the
// next link, and inserts it inside ONE transaction holding a
// pg_advisory_xact_lock. The lock is the whole point.
//
// Without it, extending a hash chain is a check-then-act. Two regulatory pods each
// read the same head, each compute chain.Next(head, theirBody), and each insert —
// producing two links with the SAME prev_hash and different hashes. The chain
// forks into two divergent histories, ON CONFLICT (hash) cannot detect it (the
// hashes differ), and every filing after the fork claims a position in a chain
// that no longer has one truth. A regulator asking "show me the unbroken chain"
// gets two answers.
//
// This is the same discipline audit.Postgres.Append already uses for the
// tamper-evident log. It is what allows regulatory to run more than one replica.
func (p *Postgres) AppendChained(ctx context.Context, body []byte) (signer.Link, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return signer.Link{}, fmt.Errorf("linkstore: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Serialize read-head -> insert against every other writer, in this pod or any
	// other. Held for the transaction; released on commit or rollback.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", chainLockKey); err != nil {
		return signer.Link{}, fmt.Errorf("linkstore: lock chain: %w", err)
	}

	prev := chain.Genesis
	err = tx.QueryRow(ctx,
		`SELECT hash FROM audit_chain_links ORDER BY seq DESC LIMIT 1`).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return signer.Link{}, fmt.Errorf("linkstore: read chain head: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		prev = chain.Genesis // fresh chain
	}

	link := signer.Link{
		Prev: prev,
		Cur:  chain.Next(prev, body),
		Body: append([]byte(nil), body...),
	}
	// Idempotent on hash: the identical filing chained at the identical head is the
	// identical link, recorded once.
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_chain_links (prev_hash, hash, canonical)
		VALUES ($1, $2, $3)
		ON CONFLICT (hash) DO NOTHING
	`, link.Prev, link.Cur, link.Body); err != nil {
		return signer.Link{}, fmt.Errorf("linkstore: append link %s: %w", link.Cur, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return signer.Link{}, fmt.Errorf("linkstore: commit link %s: %w", link.Cur, err)
	}
	return link, nil
}

// Append records one chain link, idempotent on hash (ON CONFLICT DO NOTHING).
// The seq identity column preserves append order for Head and Links.
func (p *Postgres) Append(ctx context.Context, link signer.Link) error {
	if link.Cur == "" {
		return errors.New("linkstore: cannot append a link with an empty hash")
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO audit_chain_links (prev_hash, hash, canonical)
		VALUES ($1, $2, $3)
		ON CONFLICT (hash) DO NOTHING
	`, link.Prev, link.Cur, link.Body)
	if err != nil {
		return fmt.Errorf("linkstore: append link %s: %w", link.Cur, err)
	}
	return nil
}

// Head returns the hash of the highest-seq link, or "" when the table is empty.
func (p *Postgres) Head(ctx context.Context) (string, error) {
	var head string
	err := p.pool.QueryRow(ctx, `
		SELECT hash FROM audit_chain_links ORDER BY seq DESC LIMIT 1
	`).Scan(&head)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("linkstore: read head: %w", err)
	}
	return head, nil
}

// Links returns every link in append (seq) order for chain verification.
func (p *Postgres) Links(ctx context.Context) ([]signer.Link, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT prev_hash, hash, canonical FROM audit_chain_links ORDER BY seq
	`)
	if err != nil {
		return nil, fmt.Errorf("linkstore: query links: %w", err)
	}
	defer rows.Close()

	var out []signer.Link
	for rows.Next() {
		var l signer.Link
		if err := rows.Scan(&l.Prev, &l.Cur, &l.Body); err != nil {
			return nil, fmt.Errorf("linkstore: scan link: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("linkstore: iterate links: %w", err)
	}
	return out, nil
}

var _ Store = (*Postgres)(nil)
