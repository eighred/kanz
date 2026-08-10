package migrate

// THE MIGRATE TESTS MUST NOT BE ABLE TO TOUCH THE SHARED LEDGER (#212).
//
// newPool's comment explains the defect and the fix. This file is the part that
// RUNS. #212's "Verified when" is explicit that the guarantee must be "asserted
// by something that runs, not by step ordering in a workflow" — because the only
// thing protecting CI before was that kanz-ci.yml happens to run its
// kanz-migrate step before `go test`, and nothing enforced that ordering.
//
// So this plants a sentinel in the SHARED public.schema_migrations, exercises
// the full migrate-test path against a private schema, and asserts the sentinel
// survived. If someone ever puts `public` back on the search_path, or restores a
// DROP TABLE reset, this fails — and it fails naming the row that would have
// been destroyed, which is a service's record that its schema was applied.
//
// IT IS NON-DESTRUCTIVE, deliberately. It never drops public.schema_migrations:
// if the table is absent it creates one and removes it again; if it is present —
// a developer's real ledger — it adds one row at a version no real migration
// uses and deletes exactly that row. A test that protects the ledger by
// destroying it would be its own bug report.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sentinelVersion is far above any real migration. If a directory ever legitimately
// reaches it, this test collides loudly rather than silently overwriting a row.
const sentinelVersion = 999001

func TestTheseTestsCannotReachTheSharedMigrationLedger(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to prove the migrate tests cannot reach the shared ledger")
	}
	ctx := context.Background()

	// A pool on the DEFAULT search_path — this is what every other package's
	// tests, and kanz-migrate itself, see.
	shared, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect (shared): %v", err)
	}
	// t.Cleanup, NOT defer, and registered FIRST so it runs LAST.
	//
	// Deferred functions run when the test function returns; t.Cleanup functions
	// run after that. So `defer shared.Close()` closes the pool BEFORE the
	// sentinel cleanup below, which then fails against a closed pool — and
	// because that cleanup discards its error, it fails SILENTLY and the sentinel
	// row survives in the shared ledger. Caught by running this against a real
	// Postgres and reading the table afterwards: version 999001 was still there.
	//
	// A test written to prove nothing leaks into the shared ledger, leaking into
	// the shared ledger, is the kind of thing that only shows up when the
	// assertion is made against the real database rather than the intent.
	t.Cleanup(shared.Close)

	// migrate.go's own shape, so the sentinel lives in a table indistinguishable
	// from a real ledger.
	var createdHere bool
	var exists bool
	if err := shared.QueryRow(ctx,
		`SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("probe public.schema_migrations: %v", err)
	}
	if !exists {
		if _, err := shared.Exec(ctx, `
			CREATE TABLE public.schema_migrations (
				version    BIGINT PRIMARY KEY,
				name       TEXT        NOT NULL,
				checksum   TEXT        NOT NULL,
				applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
			)`); err != nil {
			t.Fatalf("create sentinel ledger: %v", err)
		}
		createdHere = true
	}
	// FAILING TO CLEAN UP IS A TEST FAILURE, not a shrug. Residue in the shared
	// ledger is precisely what #212 is about, so a cleanup that cannot complete
	// must say so rather than discard the error — discarding it is what let the
	// sentinel survive undetected in the first place.
	t.Cleanup(func() {
		c := context.Background()
		if createdHere {
			if _, err := shared.Exec(c, `DROP TABLE IF EXISTS public.schema_migrations`); err != nil {
				t.Errorf("could not drop the ledger this test created: %v — it is now residue in the "+
					"shared database, at the exact table #212 exists to protect", err)
			}
			return
		}
		// A real ledger was already here: remove ONLY our row.
		if _, err := shared.Exec(c,
			`DELETE FROM public.schema_migrations WHERE version = $1`, sentinelVersion); err != nil {
			t.Errorf("could not remove sentinel version %d from the shared ledger: %v — it will be "+
				"read as a real applied migration by the next kanz-migrate run", sentinelVersion, err)
		}
	})

	// ON CONFLICT WITH NO TARGET, because this ledger may have either key shape
	// (#59). A ledger this test created above is keyed on (version); one a real
	// kanz-migrate run left behind — which is what CI has, since the OMS
	// migration step runs before these tests — is keyed on (set_name, version).
	// Naming `(version)` fails against the second with SQLSTATE 42P10, and the
	// sentinel this test is built around never gets planted.
	//
	// The bare form means "if any unique constraint would be violated, do
	// nothing", which is exactly the intent: do not fail if the row is somehow
	// already here.
	if _, err := shared.Exec(ctx,
		`INSERT INTO public.schema_migrations (version, name, checksum) VALUES ($1, $2, $3)
		 ON CONFLICT DO NOTHING`,
		sentinelVersion, "sentinel_212.sql", "sentinel"); err != nil {
		t.Fatalf("plant sentinel: %v", err)
	}

	// NOW RUN THE MIGRATE TESTS' OWN PATH. newPool is what every test in this
	// package uses, and Up writes a ledger — under the old code this is the exact
	// moment public.schema_migrations was dropped and repopulated with fixtures.
	pool := newPool(t)
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

	// THE ASSERTIONS.
	var stillThere bool
	if err := shared.QueryRow(ctx,
		`SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&stillThere); err != nil {
		t.Fatalf("re-probe public.schema_migrations: %v", err)
	}
	if !stillThere {
		t.Fatal("public.schema_migrations was DROPPED by the migrate tests. Every service's record " +
			"that its schema was applied is gone, and the next kanz-migrate run will re-apply 0001 " +
			"against a database that already has those objects")
	}

	var name string
	if err := shared.QueryRow(ctx,
		`SELECT name FROM public.schema_migrations WHERE version = $1`, sentinelVersion).Scan(&name); err != nil {
		t.Fatalf("the sentinel row is gone from the shared ledger: %v", err)
	}
	if name != "sentinel_212.sql" {
		t.Fatalf("the sentinel row now reads %q — the migrate tests wrote their fixtures into the "+
			"SHARED ledger, which is how version 1 ends up recorded as 0001_widgets.sql and "+
			"kanz-migrate refuses a service's real 0001", name)
	}

	// And the fixture really did land somewhere — otherwise this test would pass
	// against a migrate that silently did nothing at all.
	var fixtures int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&fixtures); err != nil {
		t.Fatalf("read the private ledger: %v", err)
	}
	if fixtures != 1 {
		t.Fatalf("the private ledger holds %d rows, want 1 — if the fixtures did not land here, this "+
			"test proves nothing about where they DID land", fixtures)
	}
}
