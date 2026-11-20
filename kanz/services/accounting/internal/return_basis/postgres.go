package returnbasis

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/eighred/kanz/internal/refdata"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool          *pgxpool.Pool
	sectorHistory *refdata.SectorHistory
}

func NewStore(pool *pgxpool.Pool, history *refdata.SectorHistory) *Store {
	return &Store{pool: pool, sectorHistory: history}
}

type Receipt struct {
	Evidence        Evidence
	Digest          string
	benchmarkDigest string
}

// Accept admits the signed source before any downstream result can use it.
// The close lock serializes competing corrections and allows exact replay.
func (s *Store) Accept(ctx context.Context, raw, signature []byte) (Receipt, error) {
	e, err := parse(raw)
	if err != nil || e.Tenant != bus.TenantIDFromContext(ctx) || e.AsOf.After(time.Now()) {
		return Receipt{}, ErrEvidence
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := lookupAuthority(ctx, tx, e.Tenant, e.Portfolio, e.Currency)
	if errors.Is(err, ErrNotFound) || (err == nil && !a.verifies(e, raw, signature)) {
		return Receipt{}, ErrEvidence
	}
	if err != nil {
		return Receipt{}, err
	}
	benchmarkDigest, err := verifyBenchmark(ctx, tx, e)
	if err != nil {
		return Receipt{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(jsonb_build_array(
		'return-basis', app_current_tenant(), $1::text, $2::text, $3::text)::text,0))`, e.Portfolio, e.Currency, e.CloseDate); err != nil {
		return Receipt{}, err
	}
	receipt := Receipt{Evidence: e, Digest: digest(raw), benchmarkDigest: benchmarkDigest}
	var existing string
	var saved, savedSignature []byte
	err = tx.QueryRow(ctx, `SELECT digest,signed_payload,signature FROM return_basis_receipts
		WHERE portfolio_id=$1 AND currency=$2 AND close_date=$3::text::date AND as_of=$4`,
		e.Portfolio, e.Currency, e.CloseDate, e.AsOf).Scan(&existing, &saved, &savedSignature)
	if err == nil {
		if existing != receipt.Digest || !bytes.Equal(saved, raw) || !bytes.Equal(savedSignature, signature) {
			return Receipt{}, ErrConflict
		}
		if err := s.enqueue(ctx, tx, receipt); err != nil {
			return Receipt{}, err
		}
		return receipt, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, err
	}
	var latest string
	var horizon time.Time
	err = tx.QueryRow(ctx, `SELECT digest,as_of FROM return_basis_receipts
		WHERE portfolio_id=$1 AND currency=$2 AND close_date=$3::text::date
		ORDER BY as_of DESC LIMIT 1`, e.Portfolio, e.Currency, e.CloseDate).Scan(&latest, &horizon)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, err
	}
	if latest != e.PreviousDigest || !e.AsOf.After(horizon) {
		return Receipt{}, ErrConflict
	}
	if err := verifyBookedFlow(ctx, tx, e); err != nil {
		return Receipt{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO return_basis_receipts (portfolio_id,currency,close_date,as_of,digest,signed_payload,signature)
		VALUES ($1,$2,$3::text::date,$4,$5,$6,$7)`, e.Portfolio, e.Currency, e.CloseDate, e.AsOf, receipt.Digest, raw, signature)
	if err != nil {
		return Receipt{}, err
	}
	if err := s.enqueue(ctx, tx, receipt); err != nil {
		return Receipt{}, err
	}
	return receipt, tx.Commit(ctx)
}

// Read opens an exact immutable edition. A later restatement has a different
// knowledge horizon. The retained signature and scope are checked again.
func (s *Store) Read(ctx context.Context, portfolio, ccy, date string, horizon time.Time) (Receipt, error) {
	if _, err := time.Parse(time.DateOnly, date); err != nil || horizon.IsZero() || horizon.Nanosecond()%1000 != 0 {
		return Receipt{}, ErrNotFound
	}
	tenant := bus.TenantIDFromContext(ctx)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := lookupAuthority(ctx, tx, tenant, portfolio, ccy)
	if err != nil {
		return Receipt{}, err
	}
	var raw, signature []byte
	var saved string
	err = tx.QueryRow(ctx, `SELECT signed_payload,signature,digest FROM return_basis_receipts
		WHERE portfolio_id=$1 AND currency=$2 AND close_date=$3::text::date AND as_of=$4`,
		portfolio, ccy, date, horizon).Scan(&raw, &signature, &saved)
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, ErrNotFound
	}
	if err != nil {
		return Receipt{}, err
	}
	e, err := parse(raw)
	if err != nil || e.Tenant != tenant || e.Portfolio != portfolio || e.Currency != ccy || e.CloseDate != date || !e.AsOf.Equal(horizon) ||
		saved != digest(raw) || !a.verifies(e, raw, signature) {
		return Receipt{}, ErrEvidence
	}
	benchmarkDigest, err := verifyBenchmark(ctx, tx, e)
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{Evidence: e, Digest: saved, benchmarkDigest: benchmarkDigest}, nil
}

func lookupAuthority(ctx context.Context, tx pgx.Tx, tenant, portfolio, ccy string) (authority, error) {
	var a authority
	err := tx.QueryRow(ctx, `SELECT issuer_id, flow_scope, public_key FROM return_basis_authorities
		WHERE tenant_id=app_current_tenant() AND tenant_id=$1 AND portfolio_id=$2 AND currency=$3`,
		tenant, portfolio, ccy).Scan(&a.issuer, &a.flowScope, &a.key)
	if errors.Is(err, pgx.ErrNoRows) {
		return authority{}, ErrNotFound
	}
	return a, err
}
