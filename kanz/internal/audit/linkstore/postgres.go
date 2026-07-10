package linkstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
