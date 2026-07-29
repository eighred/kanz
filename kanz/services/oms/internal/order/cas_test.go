package order

// Compare-and-swap tests for Store.Save (#122).
//
// #117 closed the IN-POD half of the lost-update problem with a per-order lock.
// That lock is in-process by construction, so it stops at the pod boundary:
// oms-deploy.yaml runs replicas: 2, and a submit delivered to pod A racing a
// cancel delivered to pod B is excluded by nothing. Before this change Save was
// a blind upsert that discarded its result tag, so the loser's write was
// silently applied over the winner's — a CANCELLED order overwritten back to
// FILLED, with no error, no conflict and no metric anywhere.
//
// These tests are the reason the version column exists. The Postgres one proves
// the engine arbitrates; the MemoryStore one proves the test seam cannot pass
// where production would fail — the lesson fakeBus already cost this repository
// once.

import (
	"context"
	"errors"
	"sync"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// TestPostgresSaveIsCompareAndSwap races two SEPARATE *Postgres stores over one
// pool — the closest a single process gets to two replicas — writing the same
// order from the same loaded version. Exactly one must win.
//
// Both writers here are legitimate: each read the order, each computed a next
// state, and neither can see the other. That is precisely the production
// situation, and the only honest outcome is that one is told it lost. Silent
// convergence is the bug.
//
// MUTATION PROOF (from #122's Verified-when): drop `WHERE orders.version = $4`
// from Postgres.Save and this test fails with 2 winners — which is exactly the
// behaviour that shipped before this change.
func TestPostgresSaveIsCompareAndSwap(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	// Two stores over one pool. Separate values, so nothing in-process couples
	// them — no shared mutex, no shared map, no per-order lock.
	a := NewPostgres(pool)
	b := NewPostgres(pool)

	const id = "cas-race-1"
	if err := a.Create(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW)); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Both replicas load the SAME version. This is the read that makes them
	// concurrent writers rather than sequential ones.
	_, verA, err := a.Load(ctx, id)
	if err != nil {
		t.Fatalf("load a: %v", err)
	}
	_, verB, err := b.Load(ctx, id)
	if err != nil {
		t.Fatalf("load b: %v", err)
	}
	if verA != verB {
		t.Fatalf("the two loads disagree on version (%d vs %d) — they are not racing the same state", verA, verB)
	}

	// One writer cancels, the other fills. Under the old blind upsert whichever
	// statement executed last simply won, so a cancelled order could come back
	// as filled.
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = a.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_CANCELLED), verA)
	}()
	go func() {
		defer wg.Done()
		errs[1] = b.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_FILLED), verB)
	}()
	wg.Wait()

	var winners, conflicts int
	for i, e := range errs {
		switch {
		case e == nil:
			winners++
		case errors.Is(e, ErrConflict):
			conflicts++
		default:
			t.Fatalf("writer %d got an unexpected error: %v", i, e)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("got %d winners and %d conflicts, want exactly 1 and 1\n\n"+
			"Two writers loaded the same version and both were allowed to commit. That is a LOST "+
			"UPDATE on the order store: the loser's state — a cancel, or a fill — was overwritten "+
			"with no error returned to anybody. This is the cross-replica half of #117 that the "+
			"in-process lock cannot reach.", winners, conflicts)
	}

	// The winner must have advanced the version, or the next writer races a
	// stale predicate and the CAS degrades to the blind upsert it replaced.
	_, after, err := a.Load(ctx, id)
	if err != nil {
		t.Fatalf("load after: %v", err)
	}
	if after != verA+1 {
		t.Fatalf("version = %d after one successful Save, want %d — the winning write must advance "+
			"the version or the predicate stops excluding anything", after, verA+1)
	}
}

// TestPostgresSaveRejectsStaleVersion is the sequential form: a writer holding a
// version that has since moved must be refused, not silently applied.
//
// The race test above can in principle interleave so that one writer's read
// happens after the other's write. This one cannot: the stale version is
// guaranteed by construction, so a regression cannot hide behind scheduling.
func TestPostgresSaveRejectsStaleVersion(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	st := NewPostgres(pool)

	const id = "cas-stale-1"
	if err := st.Create(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW)); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, stale, err := st.Load(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Somebody else moves the order forward.
	if err := st.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_CANCELLED), stale); err != nil {
		t.Fatalf("first save should win: %v", err)
	}

	// Our writer still holds the pre-cancel version.
	err = st.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_FILLED), stale)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale Save returned %v, want ErrConflict\n\n"+
			"A writer holding a version the store has moved past was allowed to commit. That is how "+
			"a CANCELLED order becomes FILLED again with nothing recorded.", err)
	}

	// And the cancel must have survived.
	got, _, err := st.Load(ctx, id)
	if err != nil {
		t.Fatalf("load after: %v", err)
	}
	if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("status = %v, want CANCELLED — the rejected write was applied anyway", got.GetStatus())
	}
}

// TestMemoryStoreSaveIsCompareAndSwap holds the seam to the SAME contract.
//
// This is the parity test #122 asks for, and it is not ceremony. Every
// service-level test in this package runs against MemoryStore; if the seam
// accepted a stale version while Postgres refused it, those tests would certify
// behaviour production does not have. That is the fakeBus lesson exactly: a
// green suite against a seam that cannot fail the way the real thing fails is
// not a proof.
func TestMemoryStoreSaveIsCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()

	const id = "cas-mem-1"
	if err := m.Create(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW)); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, stale, err := m.Load(ctx, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if err := m.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_CANCELLED), stale); err != nil {
		t.Fatalf("first save should win: %v", err)
	}
	if err := m.Save(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_FILLED), stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale Save on MemoryStore returned %v, want ErrConflict — the seam must fail "+
			"where Postgres fails, or the service tests running against it certify nothing", err)
	}

	got, after, err := m.Load(ctx, id)
	if err != nil {
		t.Fatalf("load after: %v", err)
	}
	if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("status = %v, want CANCELLED", got.GetStatus())
	}
	if after != stale+1 {
		t.Fatalf("version = %d, want %d — MemoryStore must advance the version like Postgres does",
			after, stale+1)
	}
}
