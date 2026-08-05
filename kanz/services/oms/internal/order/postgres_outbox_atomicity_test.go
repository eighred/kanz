package order

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
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
		AFTER INSERT ON orders
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION kanz_test_fail_at_commit()`); err != nil {
		t.Fatalf("create constraint trigger: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		if _, err := pool.Exec(c, `DROP TRIGGER IF EXISTS kanz_test_fail_at_commit ON orders`); err != nil {
			t.Errorf("drop trigger: %v — a leftover trigger fails every later admission in this schema", err)
		}
		if _, err := pool.Exec(c, `DROP FUNCTION IF EXISTS kanz_test_fail_at_commit()`); err != nil {
			t.Errorf("drop trigger function: %v", err)
		}
	})

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
