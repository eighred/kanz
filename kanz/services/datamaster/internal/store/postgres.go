package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanz-eng/kanz/services/datamaster/internal/master"
	"github.com/kanz-eng/kanz/services/datamaster/internal/pricing"
)

// PostgresGolden is the durable GoldenStore backed by golden_records
// (0001_master.sql) — the resolved record as a replace-on-write JSONB blob.
// Reuses the risk-engine persist.Postgres stance (pgxpool, RLS via app.tenant_id).
type PostgresGolden struct {
	pool *pgxpool.Pool
}

// NewPostgresGolden returns a golden store over an existing pool.
func NewPostgresGolden(pool *pgxpool.Pool) *PostgresGolden { return &PostgresGolden{pool: pool} }

func (p *PostgresGolden) Put(ctx context.Context, rec master.SecurityMaster) error {
	if rec.InstrumentID == "" {
		return errors.New("store: cannot store golden record with empty instrument_id")
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode golden record %s: %w", rec.InstrumentID, err)
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO golden_records (tenant_id, instrument_id, record, updated_at)
		VALUES (current_setting('app.tenant_id'), $1, $2, now())
		ON CONFLICT (tenant_id, instrument_id) DO UPDATE SET
			record     = EXCLUDED.record,
			updated_at = now()
	`, rec.InstrumentID, blob)
	if err != nil {
		return fmt.Errorf("put golden record %s: %w", rec.InstrumentID, err)
	}
	return nil
}

func (p *PostgresGolden) Get(ctx context.Context, instrumentID string) (master.SecurityMaster, bool, error) {
	var blob []byte
	err := p.pool.QueryRow(ctx, `
		SELECT record FROM golden_records WHERE instrument_id = $1
	`, instrumentID).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return master.SecurityMaster{}, false, nil
	}
	if err != nil {
		return master.SecurityMaster{}, false, fmt.Errorf("get golden record %s: %w", instrumentID, err)
	}
	var rec master.SecurityMaster
	if err := json.Unmarshal(blob, &rec); err != nil {
		return master.SecurityMaster{}, false, fmt.Errorf("decode golden record %s: %w", instrumentID, err)
	}
	return rec, true, nil
}

// PostgresExceptions is the durable ExceptionStore backed by exceptions +
// exception_overrides (0001_master.sql). Add is idempotent on the deterministic
// exception id (ON CONFLICT DO NOTHING — the re-detected break keeps its review
// state); Override appends to the immutable audit trail and flips the status.
type PostgresExceptions struct {
	pool *pgxpool.Pool
}

// NewPostgresExceptions returns an exception store over an existing pool.
func NewPostgresExceptions(pool *pgxpool.Pool) *PostgresExceptions {
	return &PostgresExceptions{pool: pool}
}

func (p *PostgresExceptions) Add(ctx context.Context, e pricing.Exception) error {
	if e.ID == "" {
		return errors.New("store: cannot add exception with empty id")
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO exceptions
			(tenant_id, exception_id, kind, instrument_id, detail, status, detected_at)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, exception_id) DO NOTHING
	`, e.ID, string(e.Kind), e.InstrumentID, e.Detail, string(e.Status), e.DetectedAt)
	if err != nil {
		return fmt.Errorf("add exception %s: %w", e.ID, err)
	}
	return nil
}

func (p *PostgresExceptions) AddAll(ctx context.Context, exs []pricing.Exception) error {
	for _, e := range exs {
		if err := p.Add(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (p *PostgresExceptions) Override(ctx context.Context, id, actor, reason string, chosenPrice float64, at time.Time) error {
	if actor == "" || reason == "" {
		return fmt.Errorf("store: override requires actor and reason")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin override tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM exceptions WHERE exception_id = $1 FOR UPDATE`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("store: unknown exception %q", id)
	}
	if err != nil {
		return fmt.Errorf("lock exception %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO exception_overrides (tenant_id, exception_id, actor, reason, chosen_price, overridden_at)
		VALUES (current_setting('app.tenant_id'), $1, $2, $3, $4, $5)
	`, id, actor, reason, chosenPrice, at); err != nil {
		return fmt.Errorf("append override %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE exceptions SET status = $2 WHERE exception_id = $1
	`, id, string(pricing.StatusOverridden)); err != nil {
		return fmt.Errorf("mark overridden %s: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit override %s: %w", id, err)
	}
	return nil
}

func (p *PostgresExceptions) Get(ctx context.Context, id string) (pricing.Exception, bool, error) {
	var e pricing.Exception
	var kind, status string
	err := p.pool.QueryRow(ctx, `
		SELECT exception_id, kind, instrument_id, detail, status, detected_at
		FROM exceptions WHERE exception_id = $1
	`, id).Scan(&e.ID, &kind, &e.InstrumentID, &e.Detail, &status, &e.DetectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return pricing.Exception{}, false, nil
	}
	if err != nil {
		return pricing.Exception{}, false, fmt.Errorf("get exception %s: %w", id, err)
	}
	e.Kind = pricing.ExceptionKind(kind)
	e.Status = pricing.Status(status)
	if e.Overrides, err = p.loadOverrides(ctx, id); err != nil {
		return pricing.Exception{}, false, err
	}
	return e, true, nil
}

func (p *PostgresExceptions) Open(ctx context.Context) ([]pricing.Exception, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT exception_id, kind, instrument_id, detail, status, detected_at
		FROM exceptions WHERE status = $1 ORDER BY exception_id
	`, string(pricing.StatusOpen))
	if err != nil {
		return nil, fmt.Errorf("query open exceptions: %w", err)
	}
	defer rows.Close()
	var out []pricing.Exception
	for rows.Next() {
		var e pricing.Exception
		var kind, status string
		if err := rows.Scan(&e.ID, &kind, &e.InstrumentID, &e.Detail, &status, &e.DetectedAt); err != nil {
			return nil, fmt.Errorf("scan exception: %w", err)
		}
		e.Kind = pricing.ExceptionKind(kind)
		e.Status = pricing.Status(status)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *PostgresExceptions) loadOverrides(ctx context.Context, id string) ([]pricing.Override, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT actor, reason, chosen_price, overridden_at
		FROM exception_overrides WHERE exception_id = $1 ORDER BY seq
	`, id)
	if err != nil {
		return nil, fmt.Errorf("query overrides %s: %w", id, err)
	}
	defer rows.Close()
	var out []pricing.Override
	for rows.Next() {
		var o pricing.Override
		if err := rows.Scan(&o.Actor, &o.Reason, &o.ChosenPrice, &o.At); err != nil {
			return nil, fmt.Errorf("scan override %s: %w", id, err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

var (
	_ GoldenStore    = (*PostgresGolden)(nil)
	_ ExceptionStore = (*PostgresExceptions)(nil)
)
