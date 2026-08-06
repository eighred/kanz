package migrate

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// THE ADVISORY LOCK MUST STILL BLOCK UNDER A SHORT lock_timeout (#228).
//
// pg.Migration gives kanz-migrate lock_timeout=10s so that a queued ALTER TABLE
// fails fast instead of putting every later reader behind it in Postgres's FIFO
// lock queue. But lock_timeout aborts pg_advisory_lock too, and that wait is the
// one here that is SUPPOSED to block — it is how the second replica's
// initContainer waits its turn instead of racing the first. Up therefore lifts
// the bound for exactly that statement and restores it before any DDL runs.
//
// Driven with a 250ms lock_timeout so the failure it prevents is fast to observe:
// without the `SET lock_timeout = 0`, a contended run aborts at 250ms with
// SQLSTATE 55P03 and the pod crash-loops on a sibling that is merely busy.
func TestTheAdvisoryLockWaitIgnoresTheMigrationLockTimeout(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run migrate integration tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "250"
	cfg.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// A sibling runner is mid-migration: it holds the session advisory lock.
	holder, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer holder.Release()
	if _, err := holder.Exec(context.Background(), `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		t.Fatalf("hold the advisory lock: %v", err)
	}
	defer func() {
		_, _ = holder.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
	}()

	dir := writeMigrations(t, map[string]string{
		"0001_widgets.sql": `CREATE TABLE widgets (id TEXT PRIMARY KEY);`,
	})
	migs, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// The runner's own deadline is the only thing that should end this wait —
	// exactly as kanz-migrate's -timeout flag is in production.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = New(pool).Up(ctx, migs)
	waited := time.Since(start)

	if err == nil {
		t.Fatal("Up returned while a sibling still held the advisory lock — the serialization that " +
			"stops two initContainers applying the same non-idempotent DDL is gone")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		t.Fatalf("the advisory-lock wait was aborted by lock_timeout after %v (SQLSTATE 55P03).\n\n"+
			"That turns \"a sibling replica is mid-migration\" — the normal, expected case at "+
			"replicas: 2 — into a crash-looping initContainer. Up must SET lock_timeout = 0 around "+
			"pg_advisory_lock and RESET it afterwards; the run is still bounded by kanz-migrate's "+
			"-timeout, which is this context.", waited)
	}
	if waited < 500*time.Millisecond {
		t.Errorf("Up gave up after %v, well inside the 250ms lock_timeout's shadow — it did not wait "+
			"for the lock at all", waited)
	}
	t.Logf("contended advisory lock under lock_timeout=250ms: waited %v, then ended on the runner's "+
		"own deadline with %v", waited.Round(time.Millisecond), err)
}
