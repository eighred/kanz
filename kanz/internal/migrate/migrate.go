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
	"log/slog"
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

// ErrVersionCollision means the version is already recorded under a DIFFERENT
// file name. Distinct from ErrChecksumMismatch because the remedies are
// opposites: a mismatch is fixed by adding a new migration, a collision is made
// worse by it — the database is carrying migrations from somewhere else, and
// stacking another one on top buries the reason.
var ErrVersionCollision = errors.New("migrate: version already applied by a different migration")

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
	// set namespaces this runner's migrations within schema_migrations. Empty is
	// the UNNAMED set and is what every caller got before sets existed.
	set string
	// AdoptUnnamed relabels pre-existing unnamed rows into this runner's set,
	// once. See Up for why it is not automatic.
	AdoptUnnamed bool
}

// New returns a Runner over an existing pool, migrating the UNNAMED set. The
// caller owns the pool.
//
// This is the pre-set behaviour exactly: one migration set per database, keyed
// on version alone. It stays the default because every database that exists
// today was written by it.
func New(pool *pgxpool.Pool) *Runner { return &Runner{pool: pool} }

// NewForSet returns a Runner that namespaces its migrations under set.
//
// WHY SETS EXIST (#59). schema_migrations was keyed on `version` ALONE, and all
// fourteen migration directories in this repository start at 0001 — so any two
// services sharing a database made the second exit with ErrVersionCollision at
// its initContainer. The deployment failed; it did not degrade. That is what
// forced one-database-per-service, and one database per service is what made
// twelve separate production DSNs necessary in the first place.
//
// A set name is normally the service's own ("oms", "accounting"). Two services
// in one database now collide only if they also share a set name, which is a
// configuration mistake rather than an inevitability.
func NewForSet(pool *pgxpool.Pool, set string) *Runner {
	return &Runner{pool: pool, set: set}
}

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

	// THE ONE WAIT HERE THAT IS SUPPOSED TO BLOCK, so it is the one statement that
	// must not inherit the pool's lock_timeout (#228).
	//
	// pg.Migration sets lock_timeout=10s so a queued ALTER TABLE fails fast instead
	// of putting every later reader behind it in Postgres's FIFO lock queue. But
	// lock_timeout aborts pg_advisory_lock as well — measured against Postgres 16:
	// a contended pg_advisory_lock under lock_timeout=700ms failed after 702ms with
	// SQLSTATE 55P03. Leaving it in force would turn "the second replica waits its
	// turn" into an initContainer that crash-loops whenever a sibling is mid-run.
	//
	// The wait is still bounded: kanz-migrate's -timeout flag is this ctx's
	// deadline, and its help text already says it covers the lock wait. Session
	// scope (not SET LOCAL) because there is no transaction here.
	if _, err := conn.Exec(ctx, `SET lock_timeout = 0`); err != nil {
		return nil, fmt.Errorf("lift lock_timeout for the advisory lock: %w", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return nil, fmt.Errorf("acquire advisory lock: %w", err)
	}
	// Restore it before any DDL runs: RESET reads the value the startup packet set,
	// so this returns to the profile's bound rather than to a number repeated here.
	if _, err := conn.Exec(ctx, `RESET lock_timeout`); err != nil {
		return nil, fmt.Errorf("restore lock_timeout after the advisory lock: %w", err)
	}
	defer func() {
		// Best-effort: the lock is released anyway when the session ends.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
	}()

	if err := ensureVersionTable(ctx, conn.Conn()); err != nil {
		return nil, err
	}
	// ADOPTION IS EXPLICIT, NEVER INFERRED. A ledger written before sets holds its
	// rows under the unnamed set. Asking for a NAMED set against those rows would
	// find nothing applied and re-run every migration — against a database that
	// already has the tables, so most would fail, and any that succeeded would do
	// so on production state. Refusing is the only safe default; adopting is a
	// one-time, stated act.
	if r.set != "" && !r.AdoptUnnamed {
		var unnamed int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM schema_migrations WHERE set_name = ''`).Scan(&unnamed); err != nil {
			return nil, fmt.Errorf("count unnamed migrations: %w", err)
		}
		if unnamed > 0 {
			return nil, fmt.Errorf("schema_migrations holds %d row(s) in the UNNAMED set and this run "+
				"asked for set %q: those rows were applied before migration sets existed, and treating "+
				"them as un-applied would re-run every migration against a database that already has "+
				"the tables. Re-run with -adopt-existing to relabel them into %q (do this once, for the "+
				"service that owns this database), or drop -set to keep using the unnamed set",
				unnamed, r.set, r.set)
		}
	}
	if r.set != "" && r.AdoptUnnamed {
		tag, err := conn.Exec(ctx,
			`UPDATE schema_migrations SET set_name = $1 WHERE set_name = ''`, r.set)
		if err != nil {
			return nil, fmt.Errorf("adopt unnamed migrations into set %q: %w", r.set, err)
		}
		if n := tag.RowsAffected(); n > 0 {
			// Said out loud on the one run that does it: this rewrites the ledger,
			// and a silent rewrite of deployment history is not something to find
			// out about later.
			slog.Warn("adopted pre-existing migrations into a named set",
				"set", r.set, "rows", n)
		}
	}

	applied, err := appliedChecksums(ctx, conn.Conn(), r.set)
	if err != nil {
		return nil, err
	}

	var done []Migration
	for _, m := range migs {
		if rec, ok := applied[m.Version]; ok {
			// A DIFFERENT FILE AT THIS VERSION IS NOT AN EDIT, and conflating the
			// two hands the operator the wrong instruction. "Add a new migration
			// instead of editing an applied one" is right when someone changed a
			// migration that already ran; it is actively misleading when the
			// version was recorded by a different file, because nothing was
			// edited and adding a migration fixes nothing. That case means two
			// migration sets share one database — the integration suite's own
			// fixtures and a service's real migrations, most often — and the
			// answer is to look at the database, not at the file.
			//
			// The old message reported both as the first, and named the file on
			// disk as the one that "was modified" even when the row belonged to
			// another file entirely, sending the reader to inspect something
			// blameless.
			if rec.Name != m.Name {
				return done, fmt.Errorf("%w: version %d is recorded as %q but this directory supplies %q — "+
					"nothing was edited: two migration sets are sharing one database, or a version number was reused. "+
					"Do NOT add a new migration to get past this; check what applied %q to this database first",
					ErrVersionCollision, m.Version, rec.Name, m.Name, rec.Name)
			}
			if rec.Checksum != m.Checksum {
				return done, fmt.Errorf("%w: %s (applied checksum %s, file is now %s) — add a new migration instead of editing an applied one",
					ErrChecksumMismatch, m.Name, short(rec.Checksum), short(m.Checksum))
			}
			continue // already applied, unchanged
		}
		if err := applyOne(ctx, conn.Conn(), m, r.set); err != nil {
			return done, err
		}
		done = append(done, m)
	}
	return done, nil
}

// applyOne runs one migration and records it in the SAME transaction: the schema
// change and the evidence of it are one atomic fact.
func applyOne(ctx context.Context, conn *pgx.Conn, m Migration, set string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin %s: %w", m.Name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("apply %s: %w", m.Name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (set_name, version, name, checksum) VALUES ($1, $2, $3, $4)`,
		set, m.Version, m.Name, m.Checksum,
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
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			set_name   TEXT        NOT NULL DEFAULT '',
			version    BIGINT      NOT NULL,
			name       TEXT        NOT NULL,
			checksum   TEXT        NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (set_name, version)
		)
	`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	// UPGRADE AN EXISTING LEDGER IN PLACE. Every database written before sets has
	// schema_migrations keyed on version alone, and CREATE TABLE IF NOT EXISTS
	// leaves it untouched — so without this the new INSERT would fail on a missing
	// column against precisely the databases that already hold production state.
	//
	// The existing rows become the UNNAMED set, which is what they have always
	// been: the default keeps them exactly where a plain New() will look for them.
	if _, err := conn.Exec(ctx, `
		ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS set_name TEXT NOT NULL DEFAULT ''
	`); err != nil {
		return fmt.Errorf("add set_name to schema_migrations: %w", err)
	}
	// Swap the primary key only when it is still the single-column one. Reading
	// the catalogue rather than trying and ignoring the error: a DROP CONSTRAINT
	// that silently no-ops would leave the ledger unable to hold two sets while
	// reporting success, which is the failure this whole change is about.
	if _, err := conn.Exec(ctx, `
		DO $$
		DECLARE
			pk_name text;
			pk_cols text[];
		BEGIN
			SELECT c.conname,
			       array_agg(a.attname ORDER BY k.ord)
			  INTO pk_name, pk_cols
			  FROM pg_constraint c
			  JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON TRUE
			  JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
			 WHERE c.conrelid = 'schema_migrations'::regclass
			   AND c.contype = 'p'
			 GROUP BY c.conname;

			IF pk_cols = ARRAY['version'] THEN
				EXECUTE format('ALTER TABLE schema_migrations DROP CONSTRAINT %I', pk_name);
				ALTER TABLE schema_migrations ADD PRIMARY KEY (set_name, version);
			END IF;
		END $$
	`); err != nil {
		return fmt.Errorf("re-key schema_migrations on (set_name, version): %w", err)
	}
	return nil
}

// appliedRecord is one row of schema_migrations.
//
// THE NAME IS READ, NOT JUST THE CHECKSUM, and that is the whole point of this
// type. The table has always stored the name; this function used to discard it
// and return version -> checksum alone, so a version recorded under a DIFFERENT
// FILE was indistinguishable from the same file edited — and the caller reported
// it as the latter, naming a file that had never been applied.
type appliedRecord struct {
	Name     string
	Checksum string
}

func appliedChecksums(ctx context.Context, conn *pgx.Conn, set string) (map[int64]appliedRecord, error) {
	rows, err := conn.Query(ctx,
		`SELECT version, name, checksum FROM schema_migrations WHERE set_name = $1`, set)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]appliedRecord)
	for rows.Next() {
		var (
			v    int64
			name string
			sum  string
		)
		if err := rows.Scan(&v, &name, &sum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out[v] = appliedRecord{Name: name, Checksum: sum}
	}
	return out, rows.Err()
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}
