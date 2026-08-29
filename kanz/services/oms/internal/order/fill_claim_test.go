package order

import (
	"context"
	"errors"
	"testing"

	"github.com/eighred/kanz/internal/outbox"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5/pgxpool"
)

// THE FILL CLAIM IS A STORE GUARANTEE, SO IT IS TESTED AT THE STORE (#782).
//
// ApplyFill folds a fill into the aggregate and the aggregate records only the
// cumulative result — filled_quantity and a weighted average_fill_price. Nothing
// in that state says which fills produced it, so the fold cannot tell a first
// application from a second, and a redelivery that folds a fill the order
// already contains double-counts both numbers with nothing downstream able to
// notice.
//
// order_fills is the answer, and it is the same shape position_fills has had
// since 0003: the INSERT is the claim and RowsAffected is the verdict.
//
// A SERVICE-LEVEL TEST IS NOT ENOUGH, and finding that out is why these exist. A
// redelivery of a submit for an order that is already terminal never reaches the
// fold at all — it announces the trailing outcome and returns — so a test that
// drives the service twice and compares the aggregate passes whether or not the
// dedup is there. That test was written first, passed with the dedup deleted,
// and proved only that a path which folds nothing changes nothing.

// claimFacts is one announcement, the shape a fill fold carries.
func claimFacts(orderID string) []outbox.Record {
	return []outbox.Record{{
		Subject: EventTypeFilled, EventType: EventTypeFilled, PartitionKey: orderID,
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: Domain,
		PayloadSchemaRef: "order.v1.OrderFilled:1", EventTime: t0,
		TenantID: testTenant, Payload: []byte{0x01},
	}}
}

// TestMemoryStoreRefusesAFillItAlreadyHolds pins the in-process seam. Every
// service-level test in this package runs against MemoryStore, so a seam that
// accepted a write Postgres refuses would certify behaviour production does not
// have — the reason MemoryStore implements the CAS contract too.
func TestMemoryStoreRefusesAFillItAlreadyHolds(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	const id = "claim-mem-1"
	if err := m.Create(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW), nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, ver, err := m.Load(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if err := m.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED), ver, claimFacts(id), "f1"); err != nil {
		t.Fatalf("first fold of f1: %v", err)
	}
	_, ver2, err := m.Load(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// The same fill again, at the CORRECT version — so nothing but the claim can
	// refuse it. A version conflict would prove the wrong thing.
	err = m.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_FILLED), ver2, claimFacts(id), "f1")
	if !errors.Is(err, ErrFillApplied) {
		t.Fatalf("re-folding f1 returned %v, want ErrFillApplied. Without the claim the fold lands "+
			"and filled_quantity and average_fill_price are silently double-counted", err)
	}

	// AND NOTHING WAS WRITTEN. A refusal that still advanced the version, or still
	// queued the FACT, would announce a transition that did not happen.
	after, ver3, err := m.Load(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ver3 != ver2 {
		t.Errorf("version moved from %d to %d on a refused fold", ver2, ver3)
	}
	if after.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		t.Errorf("status = %v, want the state from the FIRST fold — the refused write landed",
			after.GetStatus())
	}

	// A DIFFERENT FILL STILL FOLDS. A claim that refused everything would satisfy
	// the assertions above and break every partial execution on the platform.
	if err := m.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_FILLED), ver2, claimFacts(id), "f2"); err != nil {
		t.Fatalf("folding a DIFFERENT fill f2: %v", err)
	}
}

// TestAnEmptyFillIDClaimsNothing. Most writes are not executions — routing, a
// cancel, a marker stamp — and they pass "". Two of those in a row must both
// land: a claim keyed on the empty string would let the first write on an order
// block every later one.
func TestAnEmptyFillIDClaimsNothing(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	const id = "claim-mem-2"
	if err := m.Create(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW), nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, ver, err := m.Load(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := m.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_ROUTED), ver, nil, ""); err != nil {
		t.Fatalf("first non-fill write: %v", err)
	}
	if err := m.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_CANCELLED), ver+1, nil, ""); err != nil {
		t.Fatalf("second non-fill write returned %v — an empty fill id must claim nothing", err)
	}
}

// TestPostgresRefusesAFillItAlreadyHolds is the same contract on the production
// backend, and it is the one that matters: MemoryStore's map is a model of
// order_fills, and this is order_fills.
//
// IT ALSO PROVES THE CLAIM IS IN THE TRANSACTION. The refused write is checked
// for its state, its version AND its outbox: a claim committed separately would
// leave the FACT queued for a fold that never happened, which is the failure
// #292 exists to prevent, reached from the other side.
func TestPostgresRefusesAFillItAlreadyHolds(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	st := NewPostgres(pool)

	const id = "claim-pg-1"
	if err := st.Create(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW), nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, ver, err := st.Load(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if err := st.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED), ver, claimFacts(id), "pgf1"); err != nil {
		t.Fatalf("first fold of pgf1: %v", err)
	}
	_, ver2, err := st.Load(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	queued := outboxDepth(t, pool)

	err = st.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_FILLED), ver2, claimFacts(id), "pgf1")
	if !errors.Is(err, ErrFillApplied) {
		t.Fatalf("re-folding pgf1 returned %v, want ErrFillApplied", err)
	}

	after, ver3, err := st.Load(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if ver3 != ver2 {
		t.Errorf("version moved from %d to %d on a refused fold — the CAS ran despite the claim", ver2, ver3)
	}
	if after.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		t.Errorf("status = %v, want the state from the first fold", after.GetStatus())
	}
	if got := outboxDepth(t, pool); got != queued {
		t.Errorf("outbox depth moved from %d to %d on a refused fold — the FACT was enqueued for a "+
			"transition that did not happen, which is exactly the pair #292 made atomic", queued, got)
	}

	// A different fill still folds, on the same order.
	if err := st.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_FILLED), ver2, claimFacts(id), "pgf2"); err != nil {
		t.Fatalf("folding a DIFFERENT fill pgf2: %v", err)
	}
}

// outboxDepth counts queued records under the test tenant, so a refused write
// can be checked for what it did NOT enqueue.
func outboxDepth(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// TestAClaimRollsBackWithTheFoldThatFailed is the property that says the claim
// is IN the transaction, and it is the one that cost a fill if it were not.
//
// A claim committed on the pool rather than the tx survives a rolled-back fold.
// The order then holds a claim for a fill it never applied, and no later delivery
// can ever apply it: every retry is refused with ErrFillApplied and the execution
// is lost permanently — the fund's money moved and the aggregate never learns.
// That is strictly worse than the double-count this whole change exists to
// prevent, because a double-count is visible in the numbers and this is not.
//
// The discriminator is a CAS that fails AFTER the claim has been made. A stale
// expectedVersion does it: the claim runs, the CAS refuses, the transaction rolls
// back — and the SAME fill must still be foldable at the correct version.
//
// It was written because moving the claim from tx to pool passed every other
// test in this file.
func TestAClaimRollsBackWithTheFoldThatFailed(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	st := NewPostgres(pool)

	const id = "claim-rollback-1"
	if err := st.Create(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW), nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, ver, err := st.Load(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// A STALE version: the claim is made, then the CAS refuses.
	err = st.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_FILLED), ver+99, claimFacts(id), "rollback-f1")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale Save returned %v, want ErrConflict — this test needs the CAS to refuse "+
			"AFTER the claim, or it proves nothing about the rollback", err)
	}

	// THE SAME FILL, AT THE RIGHT VERSION. If the claim committed outside the
	// transaction it is still there, and this fold is refused forever.
	if err := st.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_FILLED), ver, claimFacts(id), "rollback-f1"); err != nil {
		t.Fatalf("re-folding after a ROLLED BACK fold returned %v, want success. The claim outlived "+
			"the transaction that made it, so this fill can never be applied by anyone — the "+
			"execution is lost, which is worse than the double-count the claim prevents", err)
	}
}

// THE READ THAT MAKES THE CLAIM REACHABLE (#798).
//
// Save's claim can only refuse a fold ApplyFill has already accepted, and
// ApplyFill refuses any fill larger than the order's remaining leaves. So a
// re-presented fill was protected only while its quantity still fit: an order for
// 100 whose 60 was folded, re-offered the venue's cumulative [60, 40], quarantined
// on OVERFILL and never folded the 40. AppliedFills is what lets adopt() skip the
// 60 before it reaches ApplyFill at all.
//
// IT IS TESTED AT THE STORE FOR THE SAME REASON THE CLAIM IS. The service-level
// test (service_redelivery_test.go) proves the skip happens; these prove the two
// stores AGREE about which fills an order holds, which is the only thing that
// makes the in-process seam a model of order_fills rather than a second answer.

// TestMemoryStoreAppliedFillsAnswersOnlyThisOrdersClaims pins the in-process seam.
func TestMemoryStoreAppliedFillsAnswersOnlyThisOrdersClaims(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	const mine, theirs = "applied-mem-1", "applied-mem-2"
	for _, id := range []string{mine, theirs} {
		if err := m.Create(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW), nil); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	// AN ORDER WITH NOTHING FOLDED ANSWERS AN EMPTY SET, NOT AN ERROR. A caller
	// that treated "no claims yet" as a fault would nack every first adoption.
	held, err := m.AppliedFills(ctx, mine)
	if err != nil {
		t.Fatalf("AppliedFills on an order with no fills: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("AppliedFills = %v on an order that has folded nothing, want empty", held)
	}

	if err := m.Save(ctx, state(mine, orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED), 0, claimFacts(mine), "mine-f1"); err != nil {
		t.Fatalf("fold mine-f1: %v", err)
	}
	if err := m.Save(ctx, state(theirs, orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED), 0, claimFacts(theirs), "theirs-f1"); err != nil {
		t.Fatalf("fold theirs-f1: %v", err)
	}

	held, err = m.AppliedFills(ctx, mine)
	if err != nil {
		t.Fatalf("AppliedFills: %v", err)
	}
	if !held["mine-f1"] {
		t.Error("mine-f1 is absent from the order that folded it — adopt() would hand it back to " +
			"ApplyFill against leaves it no longer fits, which is the over-fill quarantine #798 " +
			"exists to end")
	}
	// ANOTHER ORDER'S CLAIM MUST NOT LEAK IN. The result is only ever used to SKIP
	// a fold, so a fill this order does not hold appearing here is a fill it would
	// silently never fold at all.
	if held["theirs-f1"] {
		t.Error("another order's claim appeared in this order's applied set — the skip it drives " +
			"would drop a real execution rather than dedup one")
	}
	if len(held) != 1 {
		t.Errorf("AppliedFills = %v, want exactly {mine-f1}", held)
	}
}

// TestPostgresAppliedFillsAnswersOnlyThisOrdersClaims is the same contract on the
// production backend, and it is the one that matters: MemoryStore's map is a model
// of order_fills, and this is order_fills.
func TestPostgresAppliedFillsAnswersOnlyThisOrdersClaims(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	st := NewPostgres(pool)

	const mine, theirs = "applied-pg-1", "applied-pg-2"
	for _, id := range []string{mine, theirs} {
		if err := st.Create(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW), nil); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	held, err := st.AppliedFills(ctx, mine)
	if err != nil {
		t.Fatalf("AppliedFills on an order with no fills: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("AppliedFills = %v on an order that has folded nothing, want empty", held)
	}

	_, ver, err := st.Load(ctx, mine)
	if err != nil {
		t.Fatalf("load %s: %v", mine, err)
	}
	if err := st.Save(ctx, state(mine, orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED), ver, claimFacts(mine), "applied-pgf1"); err != nil {
		t.Fatalf("fold applied-pgf1: %v", err)
	}
	_, tver, err := st.Load(ctx, theirs)
	if err != nil {
		t.Fatalf("load %s: %v", theirs, err)
	}
	if err := st.Save(ctx, state(theirs, orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED), tver, claimFacts(theirs), "applied-pgf2"); err != nil {
		t.Fatalf("fold applied-pgf2: %v", err)
	}

	held, err = st.AppliedFills(ctx, mine)
	if err != nil {
		t.Fatalf("AppliedFills: %v", err)
	}
	if !held["applied-pgf1"] {
		t.Error("applied-pgf1 is absent from the order that folded it — the fill would be handed " +
			"back to ApplyFill and over-fill the leaves it already consumed")
	}
	if held["applied-pgf2"] {
		t.Error("another order's claim appeared in this order's applied set — the WHERE order_id " +
			"predicate is not being applied, and the skip would drop a real execution")
	}
	if len(held) != 1 {
		t.Errorf("AppliedFills = %v, want exactly {applied-pgf1}", held)
	}
}
