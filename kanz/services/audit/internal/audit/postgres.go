package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanz-eng/kanz/services/audit/internal/chain"
)

// Postgres is the durable Store backed by 0001_audit_log.sql. It mirrors the
// Memory store's semantics — append-only, idempotent on event_id, single hash
// chain — and adds DB-layer WORM (the migration's UPDATE/DELETE trigger). Reuses
// the pgxpool pattern of the other services (market-data, schema-registry).
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a store over an existing pool; the caller owns the pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// advisoryLockKey serializes appends so seq + the hash chain are assigned under
// a single writer. A fixed key (any constant) is enough — appends are the only
// contended path and are fast.
const advisoryLockKey = 0x4155444954 // "AUDIT"

func (p *Postgres) Append(ctx context.Context, r *Record) (*Record, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize the read-head→insert so concurrent appends can't fork the chain.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(advisoryLockKey)); err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}

	if existing, ok, err := getTx(ctx, tx, r.EventID); err != nil {
		return nil, err
	} else if ok {
		return existing, nil // idempotent
	}

	var seq int64
	var prev string
	err = tx.QueryRow(ctx, `SELECT seq, hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&seq, &prev)
	if errors.Is(err, pgx.ErrNoRows) {
		seq, prev = 0, chain.Genesis
	} else if err != nil {
		return nil, fmt.Errorf("head: %w", err)
	}

	stored := *r
	stored.Seq = seq + 1
	stored.PrevHashV = prev
	stored.HashV = chain.Next(prev, stored.Canonical())

	attrs, err := json.Marshal(orEmpty(stored.Attributes))
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO audit_log (seq, event_id, correlation_id, causation_id, domain,
			event_type, event_class, tenant_id, source, occurred_at, recorded_at,
			kind, summary, attributes, schema_ref, prev_hash, hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		stored.Seq, stored.EventID, stored.CorrelationID, stored.CausationID, stored.Domain,
		stored.EventType, stored.EventClass, stored.TenantID, stored.Source, stored.OccurredAt, stored.RecordedAt,
		string(stored.Kind), stored.Summary, attrs, stored.SchemaRef, stored.PrevHashV, stored.HashV)
	if err != nil {
		return nil, fmt.Errorf("insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &stored, nil
}

func (p *Postgres) Get(ctx context.Context, eventID string) (*Record, bool, error) {
	return getTx(ctx, p.pool, eventID)
}

// querier is the subset of pgx used by getTx — satisfied by both *pgxpool.Pool
// and pgx.Tx, so Get reads outside a tx and Append's dedup reads inside one.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func getTx(ctx context.Context, q querier, eventID string) (*Record, bool, error) {
	row := q.QueryRow(ctx, selectCols+` WHERE event_id = $1`, eventID)
	r, err := scanRecord(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return r, true, nil
}

func (p *Postgres) Query(ctx context.Context, f Filter) ([]*Record, error) {
	var where []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.Correlation != "" {
		add("correlation_id = $%d", f.Correlation)
	}
	if f.Tenant != "" {
		add("tenant_id = $%d", f.Tenant)
	}
	if f.Kind != "" {
		add("kind = $%d", string(f.Kind))
	}
	if f.EventType != "" {
		add("event_type = $%d", f.EventType)
	}
	if !f.Since.IsZero() {
		add("occurred_at >= $%d", f.Since)
	}
	if !f.Until.IsZero() {
		add("occurred_at <= $%d", f.Until)
	}
	sql := selectCols
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY seq ASC"
	if f.Limit > 0 {
		args = append(args, f.Limit)
		sql += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	return p.queryRows(ctx, sql, args...)
}

func (p *Postgres) All(ctx context.Context) ([]*Record, error) {
	return p.queryRows(ctx, selectCols+" ORDER BY seq ASC")
}

func (p *Postgres) Head(ctx context.Context) (Head, error) {
	var h Head
	err := p.pool.QueryRow(ctx, `SELECT seq, hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&h.Seq, &h.Hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Head{Seq: 0, Hash: chain.Genesis}, nil
	}
	return h, err
}

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

const selectCols = `SELECT seq, event_id, correlation_id, causation_id, domain, event_type,
	event_class, tenant_id, source, occurred_at, recorded_at, kind, summary, attributes,
	schema_ref, prev_hash, hash FROM audit_log`

func (p *Postgres) queryRows(ctx context.Context, sql string, args ...any) ([]*Record, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Record, 0)
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanRecord(row pgx.Row) (*Record, error) {
	var r Record
	var attrs []byte
	if err := row.Scan(&r.Seq, &r.EventID, &r.CorrelationID, &r.CausationID, &r.Domain, &r.EventType,
		&r.EventClass, &r.TenantID, &r.Source, &r.OccurredAt, &r.RecordedAt, (*string)(&r.Kind), &r.Summary, &attrs,
		&r.SchemaRef, &r.PrevHashV, &r.HashV); err != nil {
		return nil, err
	}
	if len(attrs) > 0 {
		if err := json.Unmarshal(attrs, &r.Attributes); err != nil {
			return nil, err
		}
	}
	if len(r.Attributes) == 0 {
		r.Attributes = nil
	}
	return &r, nil
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

var _ Store = (*Postgres)(nil)
