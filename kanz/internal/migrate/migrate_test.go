package migrate

// Migration-runner integration tests. Gated on TEST_POSTGRES_URL — CI provides a
// non-superuser role (kanz-ci.yml), which is also the posture the runner has in
// production: it must own its schema to run DDL under FORCE RLS.
//
// The load-bearing test is TestUpIsSafeUnderConcurrentRunners. oms-deploy.yaml
// runs replicas: 2, so TWO initContainers execute this runner against ONE
// database at the same moment. Migrations are not idempotent (CREATE TABLE /
// CREATE POLICY, no IF NOT EXISTS), so without serialization both pods apply the
// same DDL, one crashes on "relation already exists", and the rollout fails.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newPool returns a pool scoped to a PRIVATE SCHEMA that only this test can see
// (#212).
//
// THESE TESTS USED TO DROP THE SHARED MIGRATION LEDGER. They ran
// `DROP TABLE IF EXISTS schema_migrations CASCADE` against whatever
// TEST_POSTGRES_URL points at — and that database is not private to this
// package. It is the ONE database every Postgres-gated test shares, which is why
// AGENTS.md mandates `go test -p 1`. So the ledger recording every service's
// real schema was dropped by a test fixture, and the fixture's own rows
// (0001_widgets.sql) were left behind in its place.
//
// The damage was ORDER-DEPENDENT, which is why it went unnoticed:
//
//	real migrations, then the suite → the ledger is dropped. The tables still
//	  exist; the record that they were applied does not. The next kanz-migrate
//	  re-applies 0001 against a schema that already has those objects.
//	the suite, then real migrations → version 1 is occupied by 0001_widgets.sql
//	  and kanz-migrate refuses: "version 1 is recorded as 0001_widgets.sql but
//	  this directory supplies 0001_orders.sql".
//
// CI survived only by STEP ORDERING — kanz-ci.yml happens to run its OMS
// kanz-migrate step before `go test`. Nothing enforced that. Re-ordering the
// steps, or adding a second service's migrate step after the tests, would have
// reintroduced it silently. `-p 1` does not help: this is residue and
// destruction across SEQUENTIAL runs, not a concurrent-access race.
//
// A private schema is the fix. migrate.go references `schema_migrations`
// UNQUALIFIED everywhere (:237, :252, :278), so search_path decides which one it
// means — and search_path here is the test schema ALONE, deliberately without
// `public`.
//
// THE PROTECTION IS THE PAIR, and it is worth being exact about which half does
// what, because measuring it corrected a first guess. Appending `public` to the
// path on its own does NOT reintroduce the defect: CREATE TABLE IF NOT EXISTS
// targets the FIRST schema in the path, so the fixture ledger is still created
// privately and every later unqualified reference finds it first. Verified by
// mutation — that change alone leaves the isolation test green.
//
// What destroys the shared ledger is `public` on the path AND an unqualified
// DROP, because a DROP falls through to the first schema that HAS the table.
// That pair is the original defect exactly, and it is what isolation_test.go
// fails on. So the omitted `public` is defence in depth rather than the load-
// bearing half: the removed resets are, and the guard covers both.
//
// One pool per test, and every runner inside a test shares it — which is what
// TestUpIsSafeUnderConcurrentRunners needs to contend at all.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run migrate integration tests")
	}
	ctx := context.Background()
	schema := testSchemaName()

	// Bootstrapped on the DEFAULT search_path, because the schema does not exist
	// yet and a pool scoped to it could not connect.
	boot, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := boot.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		boot.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	boot.Close()

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", url, err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	// Startup-packet parameter, so it binds before the first statement rather
	// than after a SET that some connection in the pool might miss.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect (search_path=%s): %v", schema, err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgxpool.New(context.Background(), url)
		if err != nil {
			return // the schema leaks rather than the test failing on teardown
		}
		defer c.Close()
		_, _ = c.Exec(context.Background(),
			`DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	})
	// No DROP TABLE reset: a freshly created schema is already empty, and the
	// resets are what made these tests dangerous in the first place.
	return pool
}

// testSchemaName is unique per call, so two tests in one run cannot collide and
// a leaked schema from an earlier run cannot be adopted by a later one.
func testSchemaName() string {
	return fmt.Sprintf("migratetest_%d_%d", os.Getpid(), time.Now().UnixNano())
}

// writeMigrations lays out a migrations dir in the repo's existing convention:
// NNNN_name.sql, applied in sorted order.
func writeMigrations(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, sql := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(sql), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestUpAppliesInVersionOrder(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	dir := writeMigrations(t, map[string]string{
		// 0002 depends on 0001 existing, so a wrong order fails loudly.
		"0001_widgets.sql": `CREATE TABLE widgets (id TEXT PRIMARY KEY);`,
		"0002_gadgets.sql": `CREATE TABLE gadgets (id TEXT PRIMARY KEY REFERENCES widgets (id));`,
	})

	migs, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(migs) != 2 || migs[0].Version != 1 || migs[1].Version != 2 {
		t.Fatalf("load order wrong: %+v", migs)
	}
	applied, err := New(pool).Up(ctx, migs)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("applied %d, want 2", len(applied))
	}
}

func TestUpIsIdempotent(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	dir := writeMigrations(t, map[string]string{
		"0001_widgets.sql": `CREATE TABLE widgets (id TEXT PRIMARY KEY);`,
	})
	migs, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := New(pool).Up(ctx, migs); err != nil {
		t.Fatalf("first up: %v", err)
	}
	// A restarted pod re-runs the initContainer. The DDL is NOT idempotent, so
	// the runner must skip what it already applied rather than re-execute it.
	applied, err := New(pool).Up(ctx, migs)
	if err != nil {
		t.Fatalf("second up: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("re-applied %d migrations on a clean database, want 0", len(applied))
	}
}

// TestUpIsSafeUnderConcurrentRunners is why the advisory lock exists: replicas: 2
// means two initContainers race this runner against one database.
func TestUpIsSafeUnderConcurrentRunners(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	dir := writeMigrations(t, map[string]string{
		"0001_widgets.sql": `CREATE TABLE widgets (id TEXT PRIMARY KEY);`,
		"0002_gadgets.sql": `CREATE TABLE gadgets (id TEXT PRIMARY KEY REFERENCES widgets (id));`,
	})
	migs, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	const runners = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		totalAppl int
		failures  []error
		start     = make(chan struct{})
	)
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			applied, err := New(pool).Up(ctx, migs)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			totalAppl += len(applied)
		}()
	}
	close(start)
	wg.Wait()

	// Every runner must SUCCEED (a losing pod is not an error — the schema is
	// simply already there), and each migration must be applied exactly once.
	if len(failures) != 0 {
		t.Fatalf("%d/%d concurrent runners failed, want 0: %v", len(failures), runners, failures)
	}
	if totalAppl != len(migs) {
		t.Fatalf("migrations applied %d times across %d runners, want exactly %d (a double-apply crashes the rollout)", totalAppl, runners, len(migs))
	}
}

func TestUpRollsBackAFailedMigration(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	dir := writeMigrations(t, map[string]string{
		// One statement succeeds, the next is invalid: the whole file must roll
		// back, or the database is left in a half-migrated state no one can fix.
		"0001_widgets.sql": `CREATE TABLE widgets (id TEXT PRIMARY KEY); CREATE TABLE bad (id TEXT REFERENCES nonexistent (id));`,
	})
	migs, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := New(pool).Up(ctx, migs); err == nil {
		t.Fatal("up: want error on invalid migration, got nil")
	}
	// The partial work must be gone, and nothing recorded as applied.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'widgets'`).Scan(&n); err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if n != 0 {
		t.Fatal("failed migration left the widgets table behind — it was not rolled back")
	}
}

func TestUpRejectsAnEditedAppliedMigration(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	dir := writeMigrations(t, map[string]string{
		"0001_widgets.sql": `CREATE TABLE widgets (id TEXT PRIMARY KEY);`,
	})
	migs, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := New(pool).Up(ctx, migs); err != nil {
		t.Fatalf("up: %v", err)
	}

	// Someone edits a migration that production already ran. Databases that
	// applied the old text will never see the new text, so environments silently
	// diverge. Refuse, loudly.
	edited := writeMigrations(t, map[string]string{
		"0001_widgets.sql": `CREATE TABLE widgets (id TEXT PRIMARY KEY, extra TEXT);`,
	})
	migs2, err := Load(edited)
	if err != nil {
		t.Fatalf("load edited: %v", err)
	}
	_, err = New(pool).Up(ctx, migs2)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("up on edited migration: want ErrChecksumMismatch, got %v", err)
	}
}

// A DIFFERENT FILE AT AN APPLIED VERSION IS NOT AN EDIT.
//
// This is the failure a shared test database actually produces, and it is worth
// pinning because the two conditions have OPPOSITE remedies. An edited migration
// is fixed by adding a new one. A version recorded by a different file means two
// migration sets are sharing a database — the integration fixtures here and a
// service's real migrations are the pair that collide in practice — and adding a
// migration fixes nothing while burying the reason.
//
// Found by running `kanz-migrate --dir services/oms/migrations` against the
// database the suite had just used: it reported "applied migration was modified:
// 0001_orders.sql", a file that had never been applied to it. The row belonged
// to 0001_widgets.sql, written by the fixtures above. The reader was sent to
// inspect a blameless file and told to add a migration, which would have been
// the wrong action.
func TestADifferentFileAtAnAppliedVersionIsNotReportedAsAnEdit(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	first := writeMigrations(t, map[string]string{
		"0001_widgets.sql": `CREATE TABLE widgets (id TEXT PRIMARY KEY);`,
	})
	migs, err := Load(first)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := New(pool).Up(ctx, migs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A DIFFERENT directory, reusing version 1 under another name — exactly what
	// a second service's migrations look like to a database that already carries
	// someone else's.
	second := writeMigrations(t, map[string]string{
		"0001_gadgets.sql": `CREATE TABLE gadgets (id TEXT PRIMARY KEY);`,
	})
	other, err := Load(second)
	if err != nil {
		t.Fatalf("load other: %v", err)
	}
	_, err = New(pool).Up(ctx, other)
	if err == nil {
		t.Fatal("applying a different file at an already-applied version succeeded — the collision went unnoticed")
	}
	if !errors.Is(err, ErrVersionCollision) {
		t.Errorf("error is not ErrVersionCollision: %v", err)
	}
	if errors.Is(err, ErrChecksumMismatch) {
		t.Error("a version collision is reported as a checksum mismatch — the remedies are opposites, " +
			"and this one tells the reader to add a migration, which makes it worse")
	}
	// The message must name the file that WAS applied, not only the one on disk.
	if !strings.Contains(err.Error(), "0001_widgets.sql") {
		t.Errorf("error does not name the recorded file, so the reader cannot tell what put it there: %v", err)
	}
}

// NON-VACUITY for the test above: the SAME file with changed content must still
// be a checksum mismatch, so the new branch cannot swallow the case it sits in
// front of.
func TestTheSameFileWithChangedContentIsStillAChecksumMismatch(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	before := writeMigrations(t, map[string]string{
		"0001_widgets.sql": `CREATE TABLE widgets (id TEXT PRIMARY KEY);`,
	})
	migs, err := Load(before)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := New(pool).Up(ctx, migs); err != nil {
		t.Fatalf("seed: %v", err)
	}

	after := writeMigrations(t, map[string]string{
		"0001_widgets.sql": `CREATE TABLE widgets (id TEXT PRIMARY KEY, extra TEXT);`,
	})
	edited, err := Load(after)
	if err != nil {
		t.Fatalf("load edited: %v", err)
	}
	_, err = New(pool).Up(ctx, edited)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("an edited applied migration is not reported as a checksum mismatch: %v", err)
	}
	if errors.Is(err, ErrVersionCollision) {
		t.Error("an edit is reported as a version collision — the name is identical, nothing collided")
	}
}
