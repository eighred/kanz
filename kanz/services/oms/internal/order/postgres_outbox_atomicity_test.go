package order

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/services/oms/internal/outbox"
)

// THE ORDER AND ITS ANNOUNCEMENT MUST FAIL TOGETHER, NOT JUST SUCCEED TOGETHER.
//
// #292 gave admission a transactional outbox, and two tests already cover most
// of it: outbox.TestPostgresOutboxRollsBackWithItsTransaction proves Enqueue
// discards with a rolled-back tx, and TestAdmissionCommitsTheAcceptedFactWithTheOrder
// proves the happy path lands both. Neither covers the direction that actually
// distinguishes a transactional outbox from a convenient one — Create FAILING
// after the enqueue.
//
// That gap is narrow but real, and it was found by mutation rather than review:
// re-implementing the enqueue on a SECOND transaction (p.pool.Begin instead of
// the order's tx) compiles, and every existing test still passed. The outbox
// test could not see it — it exercises Enqueue directly and never calls Create.
// The admission test could not see it — with nothing rolling back, a separate
// transaction produces an identical observable result.
//
// WHY A DEFERRED CONSTRAINT TRIGGER. Create takes *pgxpool.Pool, so there is no
// seam to inject a failing Commit, and cancelling the context mid-call is a race
// with nothing to synchronise on. A DEFERRABLE INITIALLY DEFERRED constraint
// trigger fires at COMMIT — after the order INSERT and after Enqueue — which is
// exactly the window, and Postgres schedules it deterministically rather than by
// timing. The trigger is created and dropped by this test; nothing outside it
// sees the trigger, and it never runs against a migration.
//
// WHAT FAILURE LOOKS LIKE. If the enqueue ever moves off the order's
// transaction, the order row is gone and the outbox record survives — a FACT
// queued for delivery announcing an admission that never happened. That is
// strictly worse than the defect #292 set out to fix: #238's lost FACT could at
// least be recovered from committed state, whereas an orphaned announcement
// describes an order the book has never heard of.
func TestPostgresCreateFailingAtCommitLeavesNoOutboxRecord(t *testing.T) {
	pool := newPool(t) // skips unless TEST_POSTGRES_URL is set
	ctx := context.Background()
	store := NewPostgres(pool)

	const orderID = "ORD-COMMIT-FAIL"

	// Clean up any residue from an earlier run before asserting on emptiness, or
	// a leftover row would make this test pass for the wrong reason.
	if _, err := pool.Exec(ctx, `DELETE FROM outbox WHERE partition_key = $1`, orderID); err != nil {
		t.Fatalf("clear outbox: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM orders WHERE order_id = $1`, orderID); err != nil {
		t.Fatalf("clear orders: %v", err)
	}

	failOrdersAtCommit(t, pool, "INSERT")

	announce := []outbox.Record{{
		Subject:          "order.order.accepted",
		EventType:        "order.order.accepted",
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "order",
		EventTime:        time.Unix(0, 0).UTC(),
		PartitionKey:     orderID,
		PayloadSchemaRef: "order.v1.OrderState:1",
		// The tenant the FACT publishes under (envelope_tenant_id), which is not
		// the RLS storage scope — see the two-column split in migration 0006.
		TenantID: testTenant,
		// Payload is a BYTEA NOT NULL; any non-empty bytes will do, since this test
		// asserts the record's ABSENCE rather than anything about its contents.
		Payload: []byte{0x01},
	}}

	err := store.Create(ctx, state(orderID, orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW), announce)
	if err == nil {
		t.Fatal("Create returned nil with a deferred trigger that always raises — " +
			"the commit did not fail, so this run proves nothing about the failure path")
	}
	if errors.Is(err, ErrExists) || !strings.Contains(err.Error(), "forced commit failure") {
		t.Fatalf("Create failed for the wrong reason: %v — this test needs the failure to happen "+
			"at COMMIT, after the enqueue, not before it", err)
	}

	// NON-VACUITY: the trigger must have let the INSERT itself through, or the
	// enqueue was never reached and the assertion below is trivially true.
	var orders int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE order_id = $1`, orderID).Scan(&orders); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if orders != 0 {
		t.Fatalf("orders holds %d row(s) for %s after a failed commit — the transaction did not roll back",
			orders, orderID)
	}

	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE partition_key = $1`, orderID).Scan(&queued); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if queued != 0 {
		t.Fatalf("outbox holds %d record(s) for %s while the order itself does not exist — "+
			"the announcement outlived the admission it announces. The enqueue is not on the "+
			"order's transaction, so the relay will publish an ORDER_ACCEPTED for an order the "+
			"book never admitted (#292)", queued, orderID)
	}
}

// THE SAME DIRECTION FOR THE FILL FOLD, WHICH IS THE PAIR THAT MATTERS MOST.
//
//	TEST_POSTGRES_URL=… go test -p 1 -run TestPostgresSaveFailingAtCommitLeavesNoOutboxRecord ./services/oms/...
//
// Store.Save now takes []outbox.Record and work()/adopt() pass the ORDER_FILLED
// FACT with the state that records it. Everything the test above argues applies
// here unchanged, and the consequence is worse in both directions:
//
//   - Lose the FACT and the fill is GONE. The stored OrderState keeps only the
//     cumulative aggregate, so completeTerminalOutcome cannot rebuild it and
//     says so; unlike admission there is no marker and no sweep behind it.
//   - Keep the FACT after a failed write and the estate is told about an
//     execution the order does not contain — an ORDER_FILLED whose quantity the
//     order aggregate never folded, which the position book WILL fold because it
//     dedups on fill_id and this fill_id is new to it.
//
// The mutation that motivates it is the same one #295 found: enqueueing on a
// second transaction (or, here, moving the Enqueue outside the `len(announce)`
// branch onto the pool) compiles and passes every test that only checks the
// happy path.
//
// AFTER UPDATE, not AFTER INSERT: Save's compare-and-swap reaches an existing
// row through the ON CONFLICT DO UPDATE arm.
func TestPostgresSaveFailingAtCommitLeavesNoOutboxRecord(t *testing.T) {
	pool := newPool(t) // skips unless TEST_POSTGRES_URL is set
	ctx := context.Background()
	store := NewPostgres(pool)

	const orderID = "ORD-SAVE-COMMIT-FAIL"

	if err := store.Create(ctx, state(orderID, orderpb.OrderStatus_ORDER_STATUS_ROUTED), nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, ver, err := store.Load(ctx, orderID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM outbox WHERE partition_key = $1`, orderID); err != nil {
		t.Fatalf("clear outbox: %v", err)
	}

	// The trigger goes on AFTER the admission above, so the INSERT that sets this
	// test up is not the thing being failed.
	failOrdersAtCommit(t, pool, "UPDATE")

	err = store.Save(ctx, state(orderID, orderpb.OrderStatus_ORDER_STATUS_FILLED), ver, []outbox.Record{{
		Subject:          "order.order.filled",
		EventType:        "order.order.filled",
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "order",
		EventTime:        time.Unix(0, 0).UTC(),
		PartitionKey:     orderID,
		PayloadSchemaRef: "order.v1.OrderFilled:1",
		TenantID:         testTenant,
		Payload:          []byte{0x01},
	}})
	if err == nil {
		t.Fatal("Save returned nil with a deferred trigger that always raises — the commit did not " +
			"fail, so this run proves nothing about the failure path")
	}
	// The failure has to happen at COMMIT, AFTER the enqueue. A conflict would
	// return before the enqueue was ever reached and the assertion below would be
	// trivially true — the exact vacuity #295's own guards caught twice.
	if errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "forced commit failure") {
		t.Fatalf("Save failed for the wrong reason: %v — this test needs the failure at COMMIT, "+
			"after the enqueue, not before it", err)
	}

	// NON-VACUITY: the UPDATE itself must have been applied and then rolled back.
	// A version that moved would mean the rollback did not happen; a version that
	// never moved because the CAS refused is caught above.
	var (
		gotVer    int64
		gotStatus int32
	)
	if err := pool.QueryRow(ctx,
		`SELECT version, status FROM orders WHERE order_id = $1`, orderID,
	).Scan(&gotVer, &gotStatus); err != nil {
		t.Fatalf("read order back: %v", err)
	}
	if gotVer != ver || gotStatus != int32(orderpb.OrderStatus_ORDER_STATUS_ROUTED) {
		t.Fatalf("order is at version %d/status %v after a failed commit, want %d/ROUTED — "+
			"the transaction did not roll back", gotVer, orderpb.OrderStatus(gotStatus), ver)
	}

	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE partition_key = $1`, orderID).Scan(&queued); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if queued != 0 {
		t.Fatalf("outbox holds %d record(s) for %s while the fill it announces was rolled back — "+
			"the relay will publish an ORDER_FILLED for an execution the order aggregate does not "+
			"contain, and the position book, which dedups on fill_id and has never seen this one, "+
			"will fold it into the fund's position (#292)", queued, orderID)
	}
}

// failOrdersAtCommit installs a DEFERRABLE INITIALLY DEFERRED constraint trigger
// that raises when the transaction commits, for one DML event on `orders`.
//
// ONE HELPER, BOTH DIRECTIONS. Create and Save need the identical mechanism at
// different events, and a copied trigger is how the two would drift into
// proving different things — the same argument the production code makes for
// having one saveSQL.
//
// It is created and dropped by the test; nothing outside it sees the trigger,
// and it never touches a migration. A leftover would fail every later write in
// this schema, which is why the drop is an assertion rather than a best effort.
func failOrdersAtCommit(t *testing.T, pool *pgxpool.Pool, event string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION kanz_test_fail_at_commit() RETURNS trigger
		LANGUAGE plpgsql AS $fn$
		BEGIN
			RAISE EXCEPTION 'forced commit failure (test)';
		END;
		$fn$`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE CONSTRAINT TRIGGER kanz_test_fail_at_commit
		AFTER `+event+` ON orders
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION kanz_test_fail_at_commit()`); err != nil {
		t.Fatalf("create constraint trigger for %s: %v", event, err)
	}
	t.Cleanup(func() {
		c := context.Background()
		if _, err := pool.Exec(c, `DROP TRIGGER IF EXISTS kanz_test_fail_at_commit ON orders`); err != nil {
			t.Errorf("drop trigger: %v — a leftover trigger fails every later write in this schema", err)
		}
		if _, err := pool.Exec(c, `DROP FUNCTION IF EXISTS kanz_test_fail_at_commit()`); err != nil {
			t.Errorf("drop trigger function: %v", err)
		}
	})
}
