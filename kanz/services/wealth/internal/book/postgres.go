package book

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/wealth"
)

// Postgres is the durable Store backed by the 0001_book.sql schema — household
// composition as a replace-on-write JSONB projection (PARITY-02b). It reuses the
// risk-engine persist.Postgres stance (pgxpool, RLS-scoped writes via the
// app.tenant_id GUC) and preserves exact semantics: the in-memory MemoryStore
// stays the test seam, and the aggregation is unchanged.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a Postgres store over an existing pool. The caller owns
// the pool lifecycle (Close).
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Put records (or replaces) a household's composition — last-write-wins on
// household_id (a current-state projection, not a journal).
func (p *Postgres) Put(ctx context.Context, h wealth.Household) error {
	if err := h.ValidateValuation(); err != nil {
		return err
	}
	blob, err := json.Marshal(struct {
		wealth.Household
		SchemaVersion int `json:"schema_version"`
	}{h, 2})
	if err != nil {
		return fmt.Errorf("encode household %s: %w", h.HouseholdID, err)
	}
	if len(blob) > 8<<20 {
		return wealth.ErrValuation
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO households (tenant_id, household_id, composition, updated_at)
		VALUES (current_setting('app.tenant_id'), $1, $2, now())
		ON CONFLICT (tenant_id, household_id) DO UPDATE SET
			composition = EXCLUDED.composition,
			updated_at  = now()
	`, h.HouseholdID, blob)
	if err != nil {
		return fmt.Errorf("put household %s: %w", h.HouseholdID, err)
	}
	return nil
}

// Get returns a household by id; ok=false if unknown.
func (p *Postgres) Get(ctx context.Context, householdID string) (wealth.Household, bool, error) {
	var blob []byte
	err := p.pool.QueryRow(ctx, `
		SELECT composition FROM households WHERE household_id = $1
	`, householdID).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return wealth.Household{}, false, nil
	}
	if err != nil {
		return wealth.Household{}, false, fmt.Errorf("get household %s: %w", householdID, err)
	}
	var version struct {
		SchemaVersion int `json:"schema_version"`
	}
	if len(blob) > 8<<20 {
		return wealth.Household{}, false, wealth.ErrValuation
	}
	if json.Unmarshal(blob, &version) != nil || version.SchemaVersion != 2 {
		return wealth.Household{}, false, wealth.ErrLegacyPrecision
	}
	var h wealth.Household
	if err := json.Unmarshal(blob, &h); err != nil {
		return wealth.Household{}, false, fmt.Errorf("decode household %s: %w", householdID, err)
	}
	if err := h.ValidateValuation(); err != nil {
		return wealth.Household{}, false, err
	}
	return h, true, nil
}

// Compile-time assertion that Postgres satisfies Store.
var _ Store = (*Postgres)(nil)
