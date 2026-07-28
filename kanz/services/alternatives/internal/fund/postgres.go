package fund

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/alternatives"
)

// Postgres is the durable Store backed by the 0001_fund.sql schema — the
// append-only commitment journal (PARITY-02b). It reuses the risk-engine
// persist.Postgres stance (pgxpool, RLS-scoped writes via the app.tenant_id GUC)
// and preserves exact fold semantics: the in-memory MemoryStore stays the test
// seam, and alternatives.Replay over this store's Journal reconstructs the same
// Position.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a Postgres store over an existing pool. The caller owns
// the pool lifecycle (Close).
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Append records one lifecycle event, idempotent on event_id (ON CONFLICT DO
// NOTHING).
func (p *Postgres) Append(ctx context.Context, e *alternatives.Event) error {
	if e == nil || e.EventID == "" {
		return errors.New("fund: cannot append event with empty event_id")
	}
	if e.CommitmentID == "" {
		return errors.New("fund: cannot append event with empty commitment_id")
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO fund_events
			(tenant_id, event_id, commitment_id, event_type, amount, event_date)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, event_id) DO NOTHING
	`, e.EventID, e.CommitmentID, int(e.Type), amountText(e.Amount), e.Date)
	if err != nil {
		return fmt.Errorf("append event %s: %w", e.EventID, err)
	}
	return nil
}

// Journal returns a commitment's events in (date, event_id) order — the caller
// folds them via alternatives.Replay (which is order-independent, but the
// ordered read keeps the durable and in-memory paths identical).
func (p *Postgres) Journal(ctx context.Context, commitmentID string) ([]*alternatives.Event, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT event_id, commitment_id, event_type, amount, event_date
		FROM fund_events WHERE commitment_id = $1
		ORDER BY event_date, event_id
	`, commitmentID)
	if err != nil {
		return nil, fmt.Errorf("query journal %s: %w", commitmentID, err)
	}
	defer rows.Close()

	var out []*alternatives.Event
	for rows.Next() {
		var (
			e      alternatives.Event
			amount *string
		)
		if err := rows.Scan(&e.EventID, &e.CommitmentID, &e.Type, &amount, &e.Date); err != nil {
			return nil, fmt.Errorf("scan event %s: %w", commitmentID, err)
		}
		if e.Amount, err = parseAmount(amount); err != nil {
			return nil, fmt.Errorf("decode amount %s: %w", e.EventID, err)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// Commitments returns the distinct commitment ids known to the store.
func (p *Postgres) Commitments(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT DISTINCT commitment_id FROM fund_events ORDER BY commitment_id`)
	if err != nil {
		return nil, fmt.Errorf("query commitments: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan commitment: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// amountText marshals a *big.Rat to its lossless RatString, or nil (⇒ SQL NULL).
func amountText(r *big.Rat) any {
	if r == nil {
		return nil
	}
	return r.RatString()
}

func parseAmount(s *string) (*big.Rat, error) {
	if s == nil {
		return nil, nil
	}
	r, ok := new(big.Rat).SetString(*s)
	if !ok {
		return nil, fmt.Errorf("fund: malformed rational %q", *s)
	}
	return r, nil
}

// Compile-time assertion that Postgres satisfies Store.
var _ Store = (*Postgres)(nil)
