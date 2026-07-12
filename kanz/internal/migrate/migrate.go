// Package migrate applies a service's SQL migrations to Postgres, exactly once,
// in version order, under a lock that makes concurrent runners safe.
//
// It exists because nothing in this repository applied schemas. Ten services ship
// a migrations/ directory and not one of them was ever executed by any automation
// — the tables existed only because tests created them. An OMS pod deployed today
// starts, connects, and fails every order with `relation "orders" does not exist`.
//
// # Why not golang-migrate
//
// The repo already has a migration convention: every Postgres test globs
// migrations/*.sql, sorts, and executes in order. golang-migrate would mean a new
// dependency, renaming all 11 migration files to *.up.sql, and a second
// versioning scheme competing with the one the tests already use. This package
// reuses the existing convention instead — glob, sort, apply — and adds the two
// things the tests don't need but production does: durable version tracking and
// concurrency safety.
//
// # Why the advisory lock
//
// infra/deploy/oms-deploy.yaml runs replicas: 2. Both pods run this as an
// initContainer, against one database, at the same moment. The migrations are NOT
// idempotent (CREATE TABLE / CREATE POLICY, no IF NOT EXISTS), so unserialized
// runners both apply the same DDL and the loser crashes on "relation already
// exists" — a failed rollout, at deploy time, on the capital path. A session-level
// pg_advisory_lock serializes them: one applies, the rest wait and then find
// nothing to do. Losing the race is not an error; it is the expected outcome for
// every pod but one.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrChecksumMismatch means a migration file changed after it was applied.
// Databases that ran the old text will never see the new text, so environments
// silently diverge — the fix is a NEW migration, never an edit to an old one.
var ErrChecksumMismatch = errors.New("migrate: applied migration was modified")

// advisoryLockKey namespaces this runner's lock. Any constant works; it only has
// to be the same across every process that migrates this database.
const advisoryLockKey int64 = 0x6b616e7a4d494752 // "kanzMIGR"

// Migration is one .sql file: NNNN_name.sql, applied whole, in one transaction.
type Migration struct {
	Version  int64
	Name     string
	SQL      string
	Checksum string
}

// Load reads a service's migrations directory in the repo's existing convention
// — NNNN_name.sql — and returns them in ascending version order.
func Load(dir string) ([]Migration, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return nil, fmt.Errorf("glob %s: %w", dir, err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("migrate: no .sql files in %s", dir)
	}
	sort.Strings(paths)

	out := make([]Migration, 0, len(paths))
	seen := make(map[int64]string, len(paths))
	for _, p := range paths {
		base := filepath.Base(p)
		version, err := parseVersion(base)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrate: duplicate version %d (%s and %s)", version, prev, base)
		}
		seen[version] = base

		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", base, err)
		}
		sum := sha256.Sum256(b)
		out = append(out, Migration{
			Version:  version,
			Name:     base,
			SQL:      string(b),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// parseVersion pulls the leading NNNN from "0001_orders.sql".
func parseVersion(base string) (int64, error) {
	prefix, _, ok := strings.Cut(strings.TrimSuffix(base, ".sql"), "_")
	if !ok || prefix == "" {
		return 0, fmt.Errorf("migrate: %s does not match the NNNN_name.sql convention", base)
	}
	v, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("migrate: %s has a non-numeric version prefix %q", base, prefix)
	}
	return v, nil
}

// Runner applies migrations to one database.
type Runner struct {
	pool *pgxpool.Pool
}

// New returns a Runner over an existing pool. The caller owns the pool.
func New(pool *pgxpool.Pool) *Runner { return &Runner{pool: pool} }

// Up applies every migration not yet recorded, in version order, and returns
// those it applied. It is safe to run concurrently from any number of processes:
// the advisory lock serializes them, and the losers simply find nothing to do.
//
// Each migration runs in its OWN transaction together with the row recording it,
// so a migration and the fact that it ran commit or roll back as one. A failure
// leaves the database exactly as it was before that file.
func (r *Runner) Up(ctx context.Context, migs []Migration) ([]Migration, error) {
	// Hold the lock on ONE dedicated connection for the whole run — a
	// session-level lock is bound to its connection, so it cannot be taken from
	// the pool at large.
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return nil, fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		// Best-effort: the lock is released anyway when the session ends.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
	}()

	if err := ensureVersionTable(ctx, conn.Conn()); err != nil {
		return nil, err
	}
	applied, err := appliedChecksums(ctx, conn.Conn())
	if err != nil {
		return nil, err
	}

	var done []Migration
	for _, m := range migs {
		if sum, ok := applied[m.Version]; ok {
			if sum != m.Checksum {
				return done, fmt.Errorf("%w: %s (applied checksum %s, file is now %s) — add a new migration instead of editing an applied one",
					ErrChecksumMismatch, m.Name, short(sum), short(m.Checksum))
			}
			continue // already applied, unchanged
		}
		if err := applyOne(ctx, conn.Conn(), m); err != nil {
			return done, err
		}
		done = append(done, m)
	}
	return done, nil
}

// applyOne runs one migration and records it in the SAME transaction: the schema
// change and the evidence of it are one atomic fact.
func applyOne(ctx context.Context, conn *pgx.Conn, m Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin %s: %w", m.Name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("apply %s: %w", m.Name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		m.Version, m.Name, m.Checksum,
	); err != nil {
		return fmt.Errorf("record %s: %w", m.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s: %w", m.Name, err)
	}
	return nil
}

func ensureVersionTable(ctx context.Context, conn *pgx.Conn) error {
	// No RLS here: this is deployment metadata, not tenant data — the same
	// rationale as the linkstore's universal-fact store.
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    BIGINT      NOT NULL PRIMARY KEY,
			name       TEXT        NOT NULL,
			checksum   TEXT        NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`)
	if err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	return nil
}

func appliedChecksums(ctx context.Context, conn *pgx.Conn) (map[int64]string, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]string)
	for rows.Next() {
		var (
			v   int64
			sum string
		)
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out[v] = sum
	}
	return out, rows.Err()
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}
