package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (p *Postgres) Put(ctx context.Context, s Schema) error {
	if err := validatePut(&s); err != nil {
		return err
	}
	var existing string
	// The no-op SET makes RETURNING fire on the conflicting row; DO NOTHING
	// would suppress RETURNING and force a second query.
	err := p.pool.QueryRow(ctx, `
		INSERT INTO schemas (schema_id, version, descriptor, fingerprint, source_tag)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (schema_id, version)
			DO UPDATE SET source_tag = schemas.source_tag
		RETURNING fingerprint
	`, s.Ref.SchemaID, s.Ref.Version, s.Descriptor, s.Fingerprint, s.SourceTag).Scan(&existing)
	if err != nil {
		return fmt.Errorf("schema put: %w", err)
	}
	if existing != s.Fingerprint {
		return fmt.Errorf("%w: %s (have %s, got %s)", ErrConflict, s.Ref, existing, s.Fingerprint)
	}
	return nil
}

func (p *Postgres) Get(ctx context.Context, r Ref) (Schema, error) {
	var s Schema
	err := p.pool.QueryRow(ctx, `
		SELECT schema_id, version, descriptor, fingerprint, source_tag, created_at
		FROM schemas
		WHERE schema_id = $1 AND version = $2
	`, r.SchemaID, r.Version).Scan(
		&s.Ref.SchemaID, &s.Ref.Version, &s.Descriptor, &s.Fingerprint, &s.SourceTag, &s.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Schema{}, ErrNotFound
	}
	if err != nil {
		return Schema{}, fmt.Errorf("schema get: %w", err)
	}
	return s, nil
}

func (p *Postgres) Register(ctx context.Context, schemaID string, descriptor []byte, sourceTag string) (Ref, bool, error) {
	if schemaID == "" {
		return Ref{}, false, errors.New("schema id is empty")
	}
	if len(descriptor) == 0 {
		return Ref{}, false, errors.New("schema descriptor is empty")
	}
	sum := sha256.Sum256(descriptor)
	fingerprint := hex.EncodeToString(sum[:])

	// Serializable: schema registration is rare (once per release) and must
	// see a consistent "latest" to assign version = latest+1 atomically.
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Ref{}, false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var latestVer uint64
	var latestFingerprint string
	err = tx.QueryRow(ctx, `
		SELECT version, fingerprint
		FROM schemas
		WHERE schema_id = $1
		ORDER BY version DESC
		LIMIT 1
	`, schemaID).Scan(&latestVer, &latestFingerprint)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// first registration for this schema_id — fall through to insert
	case err != nil:
		return Ref{}, false, fmt.Errorf("latest lookup: %w", err)
	case latestFingerprint == fingerprint:
		if err := tx.Commit(ctx); err != nil {
			return Ref{}, false, err
		}
		return Ref{SchemaID: schemaID, Version: latestVer}, false, nil
	}

	newVer := latestVer + 1
	if _, err := tx.Exec(ctx, `
		INSERT INTO schemas (schema_id, version, descriptor, fingerprint, source_tag)
		VALUES ($1, $2, $3, $4, $5)
	`, schemaID, newVer, descriptor, fingerprint, sourceTag); err != nil {
		return Ref{}, false, fmt.Errorf("insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Ref{}, false, err
	}
	return Ref{SchemaID: schemaID, Version: newVer}, true, nil
}

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func validatePut(s *Schema) error {
	if len(s.Descriptor) == 0 {
		return errors.New("schema descriptor is empty")
	}
	if s.Ref.SchemaID == "" || s.Ref.Version == 0 {
		return errors.New("schema ref is incomplete")
	}
	if s.Fingerprint == "" {
		sum := sha256.Sum256(s.Descriptor)
		s.Fingerprint = hex.EncodeToString(sum[:])
	}
	return nil
}
