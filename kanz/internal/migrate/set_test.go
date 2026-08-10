package migrate

import (
	"context"
	"strings"
	"testing"
)

// MIGRATION SETS: TWO SERVICES, ONE DATABASE (#59).
//
// schema_migrations was keyed on `version` alone, and all fourteen migration
// directories in this repository start at 0001 — so the second service to
// migrate into a shared database exited with a version collision at its
// initContainer. The deployment failed; it did not degrade.
//
// That is what forced one database per service, and one database per service is
// what made twelve separate production DSNs necessary. These tests are the
// evidence that the constraint is retired.

func TestTwoSetsWithCollidingVersionsShareOneDatabase(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	// Both "services" start at 0001, as every real one here does, and both create
	// a table of the same name would be a different test — these differ, so the
	// only thing that can collide is the ledger.
	omsDir := writeMigrations(t, map[string]string{
		"0001_orders.sql": `CREATE TABLE orders (id TEXT PRIMARY KEY);`,
		"0002_fills.sql":  `CREATE TABLE fills (id TEXT PRIMARY KEY);`,
	})
	acctDir := writeMigrations(t, map[string]string{
		"0001_ledger_entries.sql": `CREATE TABLE ledger_entries (id TEXT PRIMARY KEY);`,
	})

	omsMigs, err := Load(omsDir)
	if err != nil {
		t.Fatalf("load oms: %v", err)
	}
	acctMigs, err := Load(acctDir)
	if err != nil {
		t.Fatalf("load accounting: %v", err)
	}

	if _, err := NewForSet(pool, "oms").Up(ctx, omsMigs); err != nil {
		t.Fatalf("oms up: %v", err)
	}
	applied, err := NewForSet(pool, "accounting").Up(ctx, acctMigs)
	if err != nil {
		t.Fatalf("accounting up into the SAME database: %v\n\n"+
			"This is the whole point of sets. Before them this returned a version "+
			"collision at 0001 and the initContainer exited, which is what forced one "+
			"database per service — and twelve production DSNs with it.", err)
	}
	if len(applied) != 1 {
		t.Fatalf("accounting applied %d migrations, want 1", len(applied))
	}

	// Both ledgers are present and separate.
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 3 {
		t.Fatalf("schema_migrations holds %d rows, want 3 (2 oms + 1 accounting)", rows)
	}
	var v1 int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version = 1`).Scan(&v1); err != nil {
		t.Fatalf("count v1: %v", err)
	}
	if v1 != 2 {
		t.Fatalf("version 1 recorded %d times, want 2 — one per set. A single row means the "+
			"primary key is still (version) and the second service overwrote or lost the first", v1)
	}
}

// A SET IS STILL IDEMPOTENT, and re-running one does not disturb the other.
func TestASetReRunsNothingAndLeavesOtherSetsAlone(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	aDir := writeMigrations(t, map[string]string{"0001_a.sql": `CREATE TABLE a (id TEXT PRIMARY KEY);`})
	bDir := writeMigrations(t, map[string]string{"0001_b.sql": `CREATE TABLE b (id TEXT PRIMARY KEY);`})
	aMigs, _ := Load(aDir)
	bMigs, _ := Load(bDir)

	if _, err := NewForSet(pool, "a").Up(ctx, aMigs); err != nil {
		t.Fatalf("a up: %v", err)
	}
	if _, err := NewForSet(pool, "b").Up(ctx, bMigs); err != nil {
		t.Fatalf("b up: %v", err)
	}
	again, err := NewForSet(pool, "a").Up(ctx, aMigs)
	if err != nil {
		t.Fatalf("a up (second run): %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("re-running set a applied %d migrations, want 0", len(again))
	}
}

// THE SAME SET NAME STILL COLLIDES, and that is correct: two services sharing a
// set name is a configuration mistake, and the collision is what says so.
func TestTheSameSetNameStillRefusesADifferentFileAtTheSameVersion(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	first := writeMigrations(t, map[string]string{"0001_a.sql": `CREATE TABLE a (id TEXT PRIMARY KEY);`})
	second := writeMigrations(t, map[string]string{"0001_b.sql": `CREATE TABLE b (id TEXT PRIMARY KEY);`})
	firstMigs, _ := Load(first)
	secondMigs, _ := Load(second)

	if _, err := NewForSet(pool, "shared").Up(ctx, firstMigs); err != nil {
		t.Fatalf("first up: %v", err)
	}
	if _, err := NewForSet(pool, "shared").Up(ctx, secondMigs); err == nil {
		t.Fatal("a DIFFERENT file at version 1 in the SAME set was accepted.\n\n" +
			"Sets namespace unrelated services; they must not hide a genuine collision " +
			"inside one service's own history.")
	}
}

// AN EXISTING LEDGER IS NOT SILENTLY RE-RUN.
//
// Every database written before sets holds its rows unnamed. Asking for a named
// set against those rows finds nothing applied — so without this guard the
// runner would re-run every migration against a database that already has the
// tables. Refusing is the only safe default.
func TestANamedSetRefusesToRunAgainstUnnamedHistory(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	dir := writeMigrations(t, map[string]string{"0001_widgets.sql": `CREATE TABLE widgets (id TEXT PRIMARY KEY);`})
	migs, _ := Load(dir)

	// The pre-set world: a plain runner, writing the unnamed set.
	if _, err := New(pool).Up(ctx, migs); err != nil {
		t.Fatalf("unnamed up: %v", err)
	}

	_, err := NewForSet(pool, "oms").Up(ctx, migs)
	if err == nil {
		t.Fatal("a named set ran against unnamed history without complaint.\n\n" +
			"It would have re-applied every migration to a database that already has " +
			"the tables — on production state, in an initContainer.")
	}
	for _, want := range []string{"UNNAMED", "adopt-existing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q, so an operator cannot act on it: %v", want, err)
		}
	}
}

// ADOPTION IS THE STATED WAY THROUGH, and it must not re-apply anything.
func TestAdoptingUnnamedHistoryRelabelsItWithoutReRunning(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	dir := writeMigrations(t, map[string]string{
		"0001_widgets.sql": `CREATE TABLE widgets (id TEXT PRIMARY KEY);`,
		"0002_gadgets.sql": `CREATE TABLE gadgets (id TEXT PRIMARY KEY);`,
	})
	migs, _ := Load(dir)
	if _, err := New(pool).Up(ctx, migs); err != nil {
		t.Fatalf("unnamed up: %v", err)
	}

	r := NewForSet(pool, "oms")
	r.AdoptUnnamed = true
	applied, err := r.Up(ctx, migs)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("adoption re-applied %d migrations, want 0.\n\n"+
			"Relabelling history must not replay it — the tables already exist.", len(applied))
	}

	var unnamed, owned int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE set_name = ''`).Scan(&unnamed); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE set_name = 'oms'`).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if unnamed != 0 || owned != 2 {
		t.Fatalf("after adoption: unnamed=%d oms=%d, want 0 and 2", unnamed, owned)
	}
}

// THE UNNAMED SET IS UNCHANGED, so every database that exists today keeps
// working with a plain New() and no flag.
func TestTheUnnamedSetBehavesExactlyAsBefore(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	dir := writeMigrations(t, map[string]string{"0001_widgets.sql": `CREATE TABLE widgets (id TEXT PRIMARY KEY);`})
	migs, _ := Load(dir)

	if _, err := New(pool).Up(ctx, migs); err != nil {
		t.Fatalf("first up: %v", err)
	}
	again, err := New(pool).Up(ctx, migs)
	if err != nil {
		t.Fatalf("second up: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("re-run applied %d, want 0", len(again))
	}
}

// THE UPGRADE PATH, AGAINST A LEDGER OF THE OLD SHAPE.
//
// Every test above starts from a table this code created, which already has
// set_name and the composite key — so none of them exercise the ALTER that real
// production databases will take. This one builds the pre-set table by hand,
// exactly as the old CREATE wrote it, records a migration in it, and then runs
// the current code over it.
//
// It is the riskiest part of the change: an upgrade that dropped the rows, or
// failed to re-key, would either re-apply every migration to a live database or
// leave the ledger unable to hold a second set while reporting success.
func TestAPreSetLedgerIsUpgradedInPlaceWithoutReplayingIt(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	// The table exactly as it was before sets existed.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE schema_migrations (
			version    BIGINT      NOT NULL PRIMARY KEY,
			name       TEXT        NOT NULL,
			checksum   TEXT        NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatalf("create the old table: %v", err)
	}

	dir := writeMigrations(t, map[string]string{
		"0001_widgets.sql": `CREATE TABLE widgets (id TEXT PRIMARY KEY);`,
	})
	migs, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Record it the way the old runner would have, and create the table it made,
	// so this really is a database with prior state rather than an empty one.
	if _, err := pool.Exec(ctx, `CREATE TABLE widgets (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("pre-existing table: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		migs[0].Version, migs[0].Name, migs[0].Checksum); err != nil {
		t.Fatalf("record the old row: %v", err)
	}

	// The current runner, unnamed — what an unchanged deployment does.
	applied, err := New(pool).Up(ctx, migs)
	if err != nil {
		t.Fatalf("up against a pre-set ledger: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("re-applied %d migration(s) against a database that already has the tables.\n\n"+
			"The upgrade lost the existing row, so every migration looked new — on production "+
			"state, in an initContainer.", len(applied))
	}

	// The column exists, the old row is in the unnamed set, and the key now
	// admits a second set.
	var setName string
	if err := pool.QueryRow(ctx, `SELECT set_name FROM schema_migrations WHERE version = 1`).Scan(&setName); err != nil {
		t.Fatalf("read set_name after upgrade: %v", err)
	}
	if setName != "" {
		t.Errorf("the pre-existing row landed in set %q, want the unnamed set", setName)
	}

	// AND THE OPERATIONAL SEQUENCE THAT FOLLOWS, asserted because it is the
	// runbook for co-tenanting a database that already exists:
	//
	//   1. the incumbent upgrades in place, unnamed, changing nothing (above);
	//   2. the incumbent ADOPTS its history into its own set, once;
	//   3. only then can a second service migrate into the same database.
	//
	// Step 2 is not optional, and the guard is why: while unnamed rows remain a
	// named set cannot tell "not yet applied" from "applied by whoever owned this
	// database first", so it refuses rather than replaying.
	otherDir := writeMigrations(t, map[string]string{"0001_other.sql": `CREATE TABLE other (id TEXT PRIMARY KEY);`})
	otherMigs, _ := Load(otherDir)

	if _, err := NewForSet(pool, "second-service").Up(ctx, otherMigs); err == nil {
		t.Fatal("a second service migrated in while the incumbent history was still unnamed — " +
			"the guard that stops a replay is not running")
	}

	incumbent := NewForSet(pool, "incumbent")
	incumbent.AdoptUnnamed = true
	if replayed, err := incumbent.Up(ctx, migs); err != nil {
		t.Fatalf("incumbent adoption: %v", err)
	} else if len(replayed) != 0 {
		t.Fatalf("adoption replayed %d migration(s) on a live database", len(replayed))
	}

	if _, err := NewForSet(pool, "second-service").Up(ctx, otherMigs); err != nil {
		t.Fatalf("a second set could not migrate into the upgraded ledger: %v\n\n"+
			"The primary key was not re-keyed, so the upgrade reported success while leaving "+
			"the database exactly as constrained as before.", err)
	}
}
