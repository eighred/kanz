// Command kanz-migrate applies a service's SQL migrations to Postgres and exits.
// It is the initContainer that runs before a service's pods start, so the schema
// a service depends on exists before the service does.
//
// It exists because nothing applied schemas. Ten services ship a migrations/
// directory; none of them was ever executed by any automation, and the tables
// existed only because tests created them. An OMS pod deployed today starts,
// connects, and fails every order with `relation "orders" does not exist`.
//
//	kanz-migrate --dir /migrations
//
// The DSN comes from KANZ_MIGRATE_DATABASE_URL, or — preferred, and what the
// deployments use — the SEC-01d Vault/CSI secret FILE named by
// KANZ_MIGRATE_DATABASE_URL_FILE. A DSN carries database credentials and does not
// belong in a pod's env block.
//
// # The migrating role is not the app role
//
// The services connect as a NON-superuser so that FORCE ROW LEVEL SECURITY
// applies to them (MT-01d). That role can run this DDL only because it OWNS its
// schema. Point this at a role with rights to create tables and policies; it is
// deliberately a separate DSN from the app's, so the blast radius of a leaked app
// credential does not include DDL.
//
// # Exit codes
//
// 0 on success, INCLUDING when there was nothing to apply — a restarted pod
// re-runs its initContainer and must not crash-loop because the schema is already
// current. Non-zero only on a real failure, which fails the pod before it can
// serve traffic against a schema it does not have.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanz-eng/kanz/internal/migrate"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "kanz-migrate: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("kanz-migrate", flag.ContinueOnError)
	dir := fs.String("dir", "/migrations", "directory of NNNN_name.sql migrations")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall deadline, including the wait for the advisory lock")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dsn := secret("KANZ_MIGRATE_DATABASE_URL")
	if dsn == "" {
		return errors.New("no DSN: set KANZ_MIGRATE_DATABASE_URL_FILE (preferred) or KANZ_MIGRATE_DATABASE_URL")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	migs, err := migrate.Load(*dir)
	if err != nil {
		return err
	}

	// The deadline covers the lock wait too: when a sibling pod is mid-migration,
	// this process BLOCKS on pg_advisory_lock until that one commits. Blocking is
	// correct — it is how the second replica waits its turn rather than racing —
	// but it must not block forever, or a stuck migration hangs the rollout with
	// no signal.
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	applied, err := migrate.New(pool).Up(ctx, migs)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		// Not an error. Every pod but the first sees this, and so does every
		// restart — crash-looping here would take the service down.
		logger.Info("schema already current", "dir", *dir, "migrations", len(migs))
		return nil
	}
	for _, m := range applied {
		logger.Info("applied migration", "version", m.Version, "name", m.Name)
	}
	logger.Info("schema up to date", "applied", len(applied), "total", len(migs))
	return nil
}

// secret resolves a sensitive value, preferring a CSI/Vault file mount (SEC-01d:
// the path in <k>_FILE) over a plaintext <k> env var — the convention the seven
// service configs already use.
func secret(k string) string {
	if p := os.Getenv(k + "_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv(k)
}
