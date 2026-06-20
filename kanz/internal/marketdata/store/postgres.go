package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
)

// Postgres is the durable Store backed by the 0001_price_history.sql schema.
// It targets plain PostgreSQL and is TimescaleDB-compatible: the migration
// turns price_observations into a hypertable when the timescaledb extension is
// present and otherwise leaves a btree-indexed regular table, so the same code
// runs on either. Reuses the schema-registry/persist pgxpool pattern (EVT-16a).
//
// # Why Timescale, not ClickHouse
//
// The board named ClickHouse/Timescale. Timescale is a Postgres extension, so
// it rides the pgx stack the platform already depends on (PERS-01, EVT-16) with
// zero new drivers — the "avoid dependency bloat" rule. ClickHouse would add a
// driver and a second operational database for marginal benefit at this scale.
//
// # Not tenant-scoped
//
// Price history is universal market fact, not tenant-owned state, so it carries
// no tenant_id and no RLS — the same call the schema registry made (its 0001
// migration): partitioning shared reference/market data per tenant would fork
// the data and break cross-tenant reads. Tenant isolation lives on the
// portfolio state that consumes these prices, not on the prices themselves.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a Postgres store over an existing pool. The caller owns
// the pool lifecycle (Close), matching the other stores.
func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

// Put inserts observations idempotently. The primary key is the full bitemporal
// identity (instrument_id, observation_time, kind, knowledge_time), so an exact
// re-put is ON CONFLICT DO NOTHING — a price at a given knowledge_time is
// immutable, and a restatement is a new knowledge_time row, never an update.
// Written in one transaction so a batch is all-or-nothing.
func (p *Postgres) Put(ctx context.Context, obs []Observation) error {
	if len(obs) == 0 {
		return nil
	}
	for i := range obs {
		if err := obs[i].validate(); err != nil {
			return err
		}
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for _, o := range obs {
		price, err := proto.Marshal(o.Price)
		if err != nil {
			return fmt.Errorf("encode price %s@%s: %w", o.InstrumentID, o.ObservationTime, err)
		}
		batch.Queue(`
			INSERT INTO price_observations
				(instrument_id, observation_time, kind, price, currency_code, knowledge_time)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (instrument_id, observation_time, kind, knowledge_time) DO NOTHING
		`, o.InstrumentID, o.ObservationTime.UTC(), int32(o.Kind), price,
			nullString(o.CurrencyCode), o.KnowledgeTime.UTC())
	}
	br := tx.SendBatch(ctx, batch)
	for range obs {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("insert observation: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("close batch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit put: %w", err)
	}
	return nil
}

// History returns the point-in-time-correct series. DISTINCT ON
// (observation_time, kind) ordered by knowledge_time DESC collapses each date
// to its latest-known version within the AsOf horizon — the SQL mirror of
// Memory's restatement collapse. The leading ORDER BY columns match the
// DISTINCT ON, so the result is already observation_time ascending.
func (p *Postgres) History(ctx context.Context, q Query) ([]Observation, error) {
	if q.InstrumentID == "" {
		return nil, errors.New("store: history with empty instrument_id")
	}
	rows, err := p.pool.Query(ctx, `
		SELECT DISTINCT ON (observation_time, kind)
		       observation_time, kind, price, currency_code, knowledge_time
		FROM price_observations
		WHERE instrument_id = $1
		  AND ($2 = 0 OR kind = $2)
		  AND ($3::timestamptz IS NULL OR observation_time >= $3)
		  AND ($4::timestamptz IS NULL OR observation_time <= $4)
		  AND ($5::timestamptz IS NULL OR knowledge_time <= $5)
		ORDER BY observation_time, kind, knowledge_time DESC
	`, q.InstrumentID, int32(q.Kind), nullTime(q.Start), nullTime(q.End), nullTime(q.AsOf))
	if err != nil {
		return nil, fmt.Errorf("query history %s: %w", q.InstrumentID, err)
	}
	defer rows.Close()

	var out []Observation
	for rows.Next() {
		o, err := scanObservation(rows, q.InstrumentID)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// LatestAsOf returns the single most recent point-in-time price, both axes
// bounded at asOf.
func (p *Postgres) LatestAsOf(ctx context.Context, instrumentID string, kind PriceKind, asOf time.Time) (Observation, bool, error) {
	if instrumentID == "" {
		return Observation{}, false, errors.New("store: latest with empty instrument_id")
	}
	row := p.pool.QueryRow(ctx, `
		SELECT observation_time, kind, price, currency_code, knowledge_time
		FROM price_observations
		WHERE instrument_id = $1 AND kind = $2
		  AND ($3::timestamptz IS NULL OR observation_time <= $3)
		  AND ($3::timestamptz IS NULL OR knowledge_time <= $3)
		ORDER BY observation_time DESC, knowledge_time DESC
		LIMIT 1
	`, instrumentID, int32(kind), nullTime(asOf))
	o, err := scanObservation(row, instrumentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Observation{}, false, nil
	}
	if err != nil {
		return Observation{}, false, err
	}
	return o, true, nil
}

// Ping checks store reachability for the readiness probe.
func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// scanRow is the shared shape of pgx.Row and a *pgx.Rows cursor position.
type scanRow interface {
	Scan(dest ...any) error
}

func scanObservation(r scanRow, instrumentID string) (Observation, error) {
	var (
		o        = Observation{InstrumentID: instrumentID}
		kind     int32
		price    []byte
		currency *string
	)
	if err := r.Scan(&o.ObservationTime, &kind, &price, &currency, &o.KnowledgeTime); err != nil {
		return Observation{}, err
	}
	o.Kind = PriceKind(kind)
	o.CurrencyCode = derefString(currency)
	var d commonpb.Decimal
	if err := proto.Unmarshal(price, &d); err != nil {
		return Observation{}, fmt.Errorf("decode price %s@%s: %w", instrumentID, o.ObservationTime, err)
	}
	o.Price = &d
	return o, nil
}

// nullTime maps the Go zero time to SQL NULL so an unset bound/horizon round-
// trips as "unbounded" rather than the epoch.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Compile-time assertion that Postgres satisfies Store.
var _ Store = (*Postgres)(nil)
