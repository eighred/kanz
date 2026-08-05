package outbox

// THE DECISIVE TESTS FOR #292 LIVE HERE, AND THEY ARE POSTGRES-GATED.
//
//	TEST_POSTGRES_URL=… go test -p 1 -run 'TestPostgresOutbox' ./services/oms/internal/outbox/
//
// Everything in relay_test.go runs against a map behind a mutex, which can model
// ordering and can model failure but CANNOT model the property this package
// exists for: that the order row and the record announcing it commit together or
// not at all. A rollback is a transaction, and only an engine has one.
//
// TestPostgresOutboxRollsBackWithItsTransaction is the load-bearing one. The
// others would pass against the in-memory queue too; that one is the reason the
// table exists.
//
// The role must be NOSUPERUSER, or the RLS assertion below passes falsely — see
// CLAUDE.md and test/backing/up.sh, which verifies it rather than assuming it.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testSchema isolates this suite's tables in their own Postgres schema, for the
// same reason services/oms/internal/order/postgres_test.go does: `go test ./...`
// runs packages in parallel against ONE database and migration 0002 rewrites the
// RLS policy on every table in current_schema().
const testSchema = "oms_outbox_test"

func newPool(t *testing.T, tenant string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run outbox Postgres integration tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "SET search_path TO "+testSchema); err != nil {
			return err
		}
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

// applySchema recreates the schema and replays the OMS migrations into it. It
// runs the REAL migration files rather than a hand-written CREATE TABLE, so a
// column this package reads and 0006 does not create fails here instead of in
// production.
func applySchema(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, os.Getenv("TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, `DROP SCHEMA IF EXISTS `+testSchema+` CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+testSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	files, err := filepath.Glob(filepath.Join("../../migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	// One connection, pinned to the schema, for the whole replay: 0002's DO block
	// resolves current_schema() at execution time.
	conn, err := admin.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET search_path TO "+testSchema); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', 'acme', false)"); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	for _, f := range files {
		ddl, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := conn.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("apply migration %s: %v", f, err)
		}
	}
}

func pgFact(t *testing.T, ctx context.Context, eventType, orderID string) Record {
	t.Helper()
	return fact(t, ctx, eventType, orderID)
}

// THE ASSERTION THE WHOLE PACKAGE EXISTS FOR: a record enqueued in a
// transaction that then ROLLS BACK is not in the table.
//
// This is what makes "the state change committed and the FACT was lost"
// unreachable rather than compensated. Its mirror — commit, and the record is
// there — is the other half, and both have to hold or the outbox is just a
// second independent write with extra steps.
//
// It cannot be written against the in-memory queue: a map behind a mutex has no
// rollback, so a test there would assert the control flow of a function and
// nothing about the engine that actually holds the fund's orders.
func TestPostgresOutboxRollsBackWithItsTransaction(t *testing.T) {
	pool := newPool(t, "acme")
	applySchema(t)
	ctx := testCtx()
	q := NewPostgres(pool)

	// 1. ENQUEUE, THEN ROLL BACK.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := Enqueue(ctx, tx, pgFact(t, ctx, "order.order.accepted", "rollback-1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	pending, err := q.Pending(ctx, "rollback-1", 10)
	if err != nil {
		t.Fatalf("Pending after rollback: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("%d records survived a rolled-back transaction — the FACT and the state change are "+
			"NOT atomic, which is the entire premise of this package", len(pending))
	}

	// 2. ENQUEUE, THEN COMMIT. The other half: a record that vanished on commit
	//    would satisfy step 1 and be catastrophic.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := Enqueue(ctx, tx, pgFact(t, ctx, "order.order.accepted", "commit-1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	pending, err = q.Pending(ctx, "commit-1", 10)
	if err != nil {
		t.Fatalf("Pending after commit: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d records after a committed transaction, want 1", len(pending))
	}
	if pending[0].Record.EventType != "order.order.accepted" {
		t.Fatalf("record round-tripped as %q", pending[0].Record.EventType)
	}
}

// A FAILED PUBLISH LEAVES THE ROW COMMITTED, THE RECORD PRESENT, AND THE FACT
// DELIVERED ONCE THE RELAY RUNS — WITHOUT A RESTART AND WITHOUT THE SWEEP.
//
// This is #292's "Verified when", run against the engine. The publisher refuses,
// so the drain fails and the record stays; the publisher recovers, so the next
// drain sends it. Nothing restarts and no compensator is involved: the FACT that
// goes out is the ORIGINAL, byte for byte, not one reconstructed from the stored
// state (which is all reannounceAccepted and completeTerminalOutcome can do —
// see their own comments on what they cannot recover).
func TestPostgresOutboxDeliversAFactWhoseFirstPublishFailed(t *testing.T) {
	pool := newPool(t, "acme")
	applySchema(t)
	ctx := testCtx()
	q := NewPostgres(pool)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := Enqueue(ctx, tx,
		pgFact(t, ctx, "order.order.accepted", "o1"),
		pgFact(t, ctx, "order.order.routed", "o1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rec := &recorder{failOn: "order.order.accepted"}
	relay, err := NewRelay(q, rec, quietLogger())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if n, err := relay.DrainOnce(ctx); err != nil || n != 0 {
		t.Fatalf("DrainOnce with the head failing = (%d, %v), want (0, nil)", n, err)
	}
	if got := rec.types(); len(got) != 0 {
		t.Fatalf("published %v while the head of the key was failing — the routed FACT must not "+
			"overtake the accepted one it belongs behind", got)
	}
	// The attempt is RECORDED, not swallowed. A record retried forever with
	// attempts stuck at zero is indistinguishable from one nothing has looked at.
	pending, err := q.Pending(ctx, "o1", 10)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 2 || pending[0].Attempts != 1 {
		t.Fatalf("pending=%d head attempts=%d, want 2 records with the head at 1 attempt",
			len(pending), pending[0].Attempts)
	}

	// The broker recovers. No restart, no sweep, no second command.
	rec.setFailOn("")
	if n, err := relay.DrainOnce(ctx); err != nil || n != 2 {
		t.Fatalf("DrainOnce after recovery = (%d, %v), want (2, nil)", n, err)
	}
	want := []string{"order.order.accepted", "order.order.routed"}
	got := rec.types()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("published %v, want %v", got, want)
	}
	if _, ok, err := q.OldestPendingAge(ctx, time.Now()); err != nil || ok {
		t.Fatalf("queue still reports pending work after a full drain (ok=%v err=%v)", ok, err)
	}
}

// THE RELAY CANNOT SEE ANOTHER TENANT'S FACTS, AND THAT IS THE MOST SERIOUS
// PROPERTY HERE.
//
// This table holds the envelopes and payloads themselves. A relay that could
// read another tenant's rows would not merely read them — it would PUBLISH them,
// under that tenant's envelope, onto the shared bus. It is also the finding that
// rules out one estate-wide relay binary: there is no unscoped pool on this
// platform to build one on, and the app role is NOSUPERUSER so it could not
// bypass RLS to reach the other tenants either.
//
// THIS TEST IS ONLY MEANINGFUL AS A NOSUPERUSER. A superuser bypasses RLS
// entirely and would see both rows, so a passing run here on a superuser role is
// a false green — the same trap CLAUDE.md names for the isolation tests.
func TestPostgresOutboxIsTenantIsolated(t *testing.T) {
	acme := newPool(t, "acme")
	applySchema(t)
	other := newPool(t, "globex")

	acmeCtx := testCtx()
	tx, err := acme.Begin(acmeCtx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := Enqueue(acmeCtx, tx, pgFact(t, acmeCtx, "order.order.accepted", "shared-id")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := tx.Commit(acmeCtx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Same partition key, different tenant. Order ids are caller-supplied, so a
	// collision across tenants is ordinary, not contrived.
	mine, err := NewPostgres(acme).Pending(acmeCtx, "shared-id", 10)
	if err != nil {
		t.Fatalf("acme Pending: %v", err)
	}
	if len(mine) != 1 {
		t.Fatalf("acme sees %d of its own records, want 1", len(mine))
	}
	theirs, err := NewPostgres(other).Pending(context.Background(), "shared-id", 10)
	if err != nil {
		t.Fatalf("globex Pending: %v", err)
	}
	if len(theirs) != 0 {
		t.Fatalf("a globex-scoped relay can read %d of acme's committed FACTs — it would PUBLISH "+
			"them under acme's envelope onto the shared bus", len(theirs))
	}
	keys, err := NewPostgres(other).PendingKeys(context.Background(), 10)
	if err != nil {
		t.Fatalf("globex PendingKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("a globex-scoped relay sees %v as pending work; those keys are acme's", keys)
	}

	// THE STORAGE TENANT AND THE ENVELOPE TENANT ARE INDEPENDENT, AND BOTH
	// BEHAVIOURS ARE REQUIRED.
	//
	// A record whose FACT belongs to another tenant is STORED here and READABLE
	// here: on today's deployment one __system__ OMS legitimately holds records
	// destined for many envelope tenants, because the api-gateway stamps the
	// caller's tenant while OMS_TENANT is __system__ (bus.RequireTenantScope,
	// #223, until #97). A test that forbade this would be pinning the outage
	// this design exists to avoid.
	//
	// What must NOT change is the storage scope: the row still lands under the
	// CONNECTION's tenant, so the globex-scoped relay above still cannot see it,
	// and the envelope tenant round-trips unmodified so the FACT publishes under
	// the fund it belongs to.
	foreign := pgFact(t, acmeCtx, "order.order.accepted", "for-globex")
	foreign.TenantID = "globex"
	tx, err = acme.Begin(acmeCtx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := Enqueue(acmeCtx, tx, foreign); err != nil {
		t.Fatalf("an acme-scoped OMS could not enqueue a FACT destined for tenant globex: %v — on "+
			"the current deployment EVERY real order is this shape (OMS_TENANT=__system__, caller "+
			"tenant on the envelope), so this failing is a total admission outage", err)
	}
	if err := tx.Commit(acmeCtx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	back, err := NewPostgres(acme).Pending(acmeCtx, "for-globex", 10)
	if err != nil || len(back) != 1 {
		t.Fatalf("Pending for the foreign-envelope record = (%d, %v), want 1", len(back), err)
	}
	if back[0].Record.TenantID != "globex" {
		t.Fatalf("envelope tenant round-tripped as %q, want globex — the relay would publish this "+
			"FACT under the wrong fund's name", back[0].Record.TenantID)
	}
	stillHidden, err := NewPostgres(other).Pending(context.Background(), "for-globex", 10)
	if err != nil {
		t.Fatalf("globex Pending: %v", err)
	}
	if len(stillHidden) != 0 {
		t.Fatalf("a globex-scoped relay can read %d rows an acme-scoped OMS wrote. The ENVELOPE "+
			"tenant is not the STORAGE tenant: RLS scopes the row to the connection that wrote it, "+
			"and two relays must never both publish one record", len(stillHidden))
	}
}

// TWO RELAYS OVER ONE TABLE, AT THE ENGINE. oms-deploy.yaml runs replicas: 2, so
// this is the normal case. The advisory lock is what makes the per-key ordering
// hold across pods; relay_test.go pins the same property in-process, where the
// lock is a map entry rather than a cluster-global lock Postgres arbitrates.
func TestPostgresOutboxLetsOnlyOneRelayDrainAKey(t *testing.T) {
	pool := newPool(t, "acme")
	applySchema(t)
	ctx := testCtx()
	q := NewPostgres(pool)

	release, ok, err := q.LockKey(ctx, "o1", false)
	if err != nil || !ok {
		t.Fatalf("first LockKey = (%v, %v), want the lock", ok, err)
	}
	// A SECOND CONNECTION, because an advisory lock is per SESSION: taking it
	// twice on one connection succeeds (Postgres locks are re-entrant per
	// session) and would prove nothing about two pods.
	second := newPool(t, "acme")
	if _, ok, err := NewPostgres(second).LockKey(ctx, "o1", false); err != nil || ok {
		t.Fatalf("a second session took a key the first holds (ok=%v err=%v) — two relays would "+
			"publish one order's FACTs concurrently and could interleave them", ok, err)
	}
	release()
	release2, ok, err := NewPostgres(second).LockKey(ctx, "o1", false)
	if err != nil || !ok {
		t.Fatalf("the key was not released (ok=%v err=%v) — a stranded lock means nothing ever "+
			"drains this order again", ok, err)
	}
	release2()
}

// CONCURRENT ENQUEUES FOR ONE KEY PUBLISH IN ORDER. The id is a BIGSERIAL and a
// sequence allocates at INSERT time rather than COMMIT time, so this asserts the
// argument written on Queue: for one partition key the committed rows are
// strictly sequential, because every enqueue rides a versioned write of the same
// aggregate and two such writes cannot both commit.
//
// Here that serialization is supplied by the drain lock rather than by
// orders.version — this package has no aggregate — so what it proves is narrower
// than the argument: that N concurrent enqueues followed by a drain publish each
// record exactly once, none lost and none duplicated.
func TestPostgresOutboxPublishesEveryConcurrentEnqueueExactlyOnce(t *testing.T) {
	pool := newPool(t, "acme")
	applySchema(t)
	ctx := testCtx()

	const writers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			tx, err := pool.Begin(ctx)
			if err != nil {
				errs[i] = err
				return
			}
			if err := Enqueue(ctx, tx, pgFact(t, ctx, "order.order.accepted", "o1")); err != nil {
				errs[i] = err
				_ = tx.Rollback(ctx)
				return
			}
			errs[i] = tx.Commit(ctx)
		}(i)
	}
	close(start)
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("concurrent enqueue: %v", err)
	}

	rec := &recorder{}
	relay, err := NewRelay(NewPostgres(pool), rec, quietLogger())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	n, err := relay.DrainOnce(ctx)
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if n != writers || len(rec.types()) != writers {
		t.Fatalf("drained %d and published %d of %d records", n, len(rec.types()), writers)
	}
	// A SECOND PASS MUST PUBLISH NOTHING. A published_at that did not stick would
	// have the relay republish every FACT on every tick, forever.
	if again, err := relay.DrainOnce(ctx); err != nil || again != 0 {
		t.Fatalf("a second drain published %d records (err %v) — published_at is not sticking", again, err)
	}
}
