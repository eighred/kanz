package state_test

// RUNTIME ACQUIRE AGAINST A REAL POSTGRES (#110).
//
// The DB-free properties are in ownership_test.go. This file exists because the
// thing #110's ruling actually bought is "a replica can load a portfolio it
// never booted with" — and a fake loader cannot prove that. It proves the loop
// end to end against the durable store the engine really runs on: save a
// portfolio, hand it to a replica that has never seen it, watch the replica
// refuse before the acquire and answer after it, then release it back and hand
// it on with its tail intact.
//
// Gated on TEST_POSTGRES_URL like the persist tests, and it skips silently
// otherwise — which is why the PR that landed it says whether it ran. The role
// must be NOSUPERUSER or the tenant-isolation case below passes falsely.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/state"
	"github.com/eighred/kanz/internal/risk/state/persist"
)

const pgMigrationDir = "../../../services/risk-engine/migrations"

// pgPool returns a pool whose every connection pins app.tenant_id to tenant —
// the authenticated-session GUC pattern the composition root uses, and the
// thing RLS scopes every read and write to.
func pgPool(t *testing.T, tenant string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run the runtime-acquire Postgres tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// applyStateSchema recreates the risk-engine state tables from the migrations
// so each run starts clean. The migration files are the source of truth,
// including the RLS policies.
func applyStateSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS applied_keys, positions, portfolios CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(pgMigrationDir, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations %s: %v (found %d)", pgMigrationDir, err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		ddl, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read migration %s: %v", f, rerr)
		}
		if _, eerr := pool.Exec(ctx, string(ddl)); eerr != nil {
			t.Fatalf("apply migration %s: %v", f, eerr)
		}
	}
}

// TestRuntimeAcquireLoadsFromPostgresAndSurvivesHandoff is the whole point of
// #110's ruling in one test: a replica that booted without a portfolio gains
// it, serves it from durable state, gives it up, and hands its tail to the
// next owner — none of which the boot-only Restore path could do.
func TestRuntimeAcquireLoadsFromPostgresAndSurvivesHandoff(t *testing.T) {
	pool := pgPool(t, "__system__")
	applyStateSchema(t, pool)
	sink := persist.NewPostgres(pool)
	ctx := context.Background()

	const id v1.PortfolioID = "PORT-ACQ"
	seed := durableRecord(id)
	if err := sink.Save(ctx, seed); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	// A replica that has never seen this portfolio. It boots with nothing —
	// no LoadAll, no Restore — which is exactly the mid-life ownership gain.
	replicaA := state.NewStore(state.WithRuntimeOwnership(sink))

	// BEFORE THE ACQUIRE IT MUST REFUSE. Not an empty book, not a zero.
	if _, err := replicaA.SnapshotOwned(id); !errors.Is(err, state.ErrNotOwned) {
		t.Fatalf("SnapshotOwned before Acquire = %v, want ErrNotOwned", err)
	}
	if err := replicaA.ApplyPortfolioRevalued(ctx, env("pre-acq"), revalued(string(id))); !errors.Is(err, state.ErrNotOwned) {
		t.Fatalf("apply before Acquire = %v, want ErrNotOwned", err)
	}

	if err := replicaA.Acquire(ctx, id); err != nil {
		t.Fatalf("Acquire from Postgres: %v", err)
	}
	got, err := replicaA.SnapshotOwned(id)
	if err != nil {
		t.Fatalf("SnapshotOwned after Acquire: %v", err)
	}
	if got.DisplayName() != seed.DisplayName {
		t.Errorf("DisplayName=%q want %q — the acquire did not read the durable record", got.DisplayName(), seed.DisplayName)
	}
	if len(got.Positions()) != len(seed.Positions) {
		t.Errorf("positions=%d want %d — a runtime-acquired portfolio must arrive with its book", len(got.Positions()), len(seed.Positions))
	}

	// The applied-key tail came from applied_keys, so a replayed boundary event
	// is skipped rather than double-counted on the new owner.
	if aerr := replicaA.ApplyPortfolioRevalued(ctx, env(seed.AppliedKeys[0]), revalued(string(id))); aerr != nil {
		t.Fatalf("apply of a persisted key: %v", aerr)
	}
	if p, serr := replicaA.SnapshotOwned(id); serr != nil {
		t.Fatalf("SnapshotOwned: %v", serr)
	} else if p.DisplayName() != seed.DisplayName {
		t.Errorf("DisplayName=%q — the dedup window was not seeded from applied_keys", p.DisplayName())
	}

	// Live traffic moves the in-memory copy AHEAD of the durable record.
	if aerr := replicaA.ApplyPortfolioRevalued(ctx, env("live-after-acq"), revalued(string(id))); aerr != nil {
		t.Fatalf("live apply: %v", aerr)
	}

	// HANDOFF. Release hands back the state it dropped, in one operation, so
	// the tail that is not yet durable cannot be lost between reading and
	// releasing.
	rec, released, err := replicaA.Release(id)
	if err != nil || !released {
		t.Fatalf("Release = (%v, %v), want (nil, true)", err, released)
	}
	if rec.DisplayName != "Live" {
		t.Fatalf("released record DisplayName=%q, want Live — the undurable tail was dropped", rec.DisplayName)
	}
	if _, serr := replicaA.SnapshotOwned(id); !errors.Is(serr, state.ErrNotOwned) {
		t.Errorf("SnapshotOwned after Release = %v, want ErrNotOwned", serr)
	}
	if serr := sink.Save(ctx, rec); serr != nil {
		t.Fatalf("checkpoint the released record: %v", serr)
	}

	// The next owner picks it up from Postgres with the tail included.
	replicaB := state.NewStore(state.WithRuntimeOwnership(sink))
	if aerr := replicaB.Acquire(ctx, id); aerr != nil {
		t.Fatalf("second replica Acquire: %v", aerr)
	}
	next, err := replicaB.SnapshotOwned(id)
	if err != nil {
		t.Fatalf("second replica SnapshotOwned: %v", err)
	}
	if next.DisplayName() != "Live" {
		t.Errorf("second replica DisplayName=%q, want Live — the handoff lost the previous owner's applies", next.DisplayName())
	}
	// And the durable record is still there after both releases: Postgres is
	// the record, memory is the copy.
	if _, _, rerr := replicaB.Release(id); rerr != nil {
		t.Fatalf("second Release: %v", rerr)
	}
	if _, lerr := sink.Load(ctx, id); lerr != nil {
		t.Errorf("durable record after Release: %v — release must not touch durable state", lerr)
	}
}

// TestRuntimeAcquireIsTenantScoped proves the acquire reads through RLS rather
// than around it: a replica authenticated as another tenant acquires the id and
// finds NOTHING, instead of loading the first tenant's book. Requires a
// NOSUPERUSER role — a superuser bypasses RLS and this passes falsely.
func TestRuntimeAcquireIsTenantScoped(t *testing.T) {
	owner := pgPool(t, "tenant-owner")
	applyStateSchema(t, owner)
	if err := persist.NewPostgres(owner).Save(context.Background(), durableRecord("PORT-TEN")); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	other := persist.NewPostgres(pgPool(t, "tenant-other"))
	s := state.NewStore(state.WithRuntimeOwnership(other))
	ctx := context.Background()
	if err := s.Acquire(ctx, "PORT-TEN"); err != nil {
		t.Fatalf("Acquire under a foreign tenant: %v", err)
	}
	// Owned, and honestly empty — the acquire found no row it was entitled to.
	_, err := s.SnapshotOwned("PORT-TEN")
	if !errors.Is(err, v1.ErrPortfolioNotFound) {
		t.Fatalf("SnapshotOwned across tenants = %v, want ErrPortfolioNotFound — a runtime acquire must not read another tenant's book", err)
	}
}
