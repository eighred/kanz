package order

// THE DEATH (#539) — a held order nobody signs says so, instead of vanishing.
//
// service_dual_control_hold_test.go proves an order over the threshold is held.
// service_dual_control_approve_test.go proves a second signature releases it.
// This is the third branch, and until now it was the silent one: a proposal
// nobody acted on left the pending queue at its deadline and entered no other
// queue, so the trader saw ORDER_PENDING_APPROVAL and then nothing, ever — no
// rejection, no outcome, no order. OrderPendingApproval's own doc promises the
// opposite, and this is what makes the promise true.

import (
	"context"
	"testing"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dualcontrol"
)

// rejectionsFor counts ORDER_REJECTED publishes carrying code. Counted here
// rather than added to fakeBus: the double is shared by every test in this
// package and widening it for one assertion is how a test helper becomes an
// interface nobody owns.
func rejectionsFor(fb *fakeBus, code string) int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	n := 0
	for _, e := range fb.events {
		if e.EventType != EventTypeRejected {
			continue
		}
		if rej, ok := e.Payload.(*orderpb.OrderRejected); ok && rej.GetErrorCode() == code {
			n++
		}
	}
	return n
}

// pastTTL moves the service's clock beyond the proposal deadline. Ageing the
// clock rather than rewriting expires_at is deliberate: expiry is a property of
// time passing, and a test that edited the row would prove the column is read
// rather than that the deadline is enforced.
func pastTTL(svc *Service) time.Time {
	at := time.Now().UTC().Add(dualcontrol.DefaultTTL + time.Hour)
	svc.now = func() time.Time { return at }
	return at
}

// THE TEST THIS CHANGE EXISTS TO MAKE PASS.
func TestAnExpiredProposalIsAnnouncedAsRejected(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")
	pastTTL(svc)

	if err := svc.ExpireProposals(testCtx()); err != nil {
		t.Fatalf("ExpireProposals: %v", err)
	}

	msg := fb.last(EventTypeRejected)
	if msg == nil {
		t.Fatalf("no ORDER_REJECTED for an expired proposal (published: %v) — the order neither "+
			"executed nor reported why, which is the failure #410's acceptance names", fb.types())
	}
	rej, ok := msg.(*orderpb.OrderRejected)
	if !ok {
		t.Fatalf("payload is %T, not OrderRejected", msg)
	}
	if rej.GetOrderId() != cmd.GetOrderId() {
		t.Errorf("order_id = %q, want %q", rej.GetOrderId(), cmd.GetOrderId())
	}
	if rej.GetErrorCode() != ReasonApprovalExpired {
		t.Errorf("error_code = %q, want %q — an examiner cannot tell an unsigned order from one "+
			"compliance refused, and those are different findings", rej.GetErrorCode(), ReasonApprovalExpired)
	}
}

// THE OUTCOME IS THE HALF THE SUBMITTER IS ACTUALLY WAITING ON. A held order
// produces no CommandOutcome when it is held — deliberately, since all four
// statuses are terminal. Its expiry is one of the two events that finally
// produces one; without this the caller waits forever on a command that was
// answered by a timeout nobody published.
func TestAnExpiredProposalProducesACommandOutcome(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")
	pastTTL(svc)

	if err := svc.ExpireProposals(testCtx()); err != nil {
		t.Fatalf("ExpireProposals: %v", err)
	}

	msg := fb.last(EventTypeOutcome)
	if msg == nil {
		t.Fatalf("no CommandOutcome for an expired proposal (published: %v)", fb.types())
	}
	out, ok := msg.(*commandpb.CommandOutcome)
	if !ok {
		t.Fatalf("payload is %T, not CommandOutcome", msg)
	}
	if out.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Errorf("status = %v, want REJECTED", out.GetStatus())
	}
	if out.GetErrorCode() != ReasonApprovalExpired {
		t.Errorf("error_code = %q, want %q", out.GetErrorCode(), ReasonApprovalExpired)
	}
	_ = cmd
}

// AND IT IS STILL NOT AN ORDER. The whole placement ruling was that a held order
// is absent from the book; its expiry must not quietly create one to reject.
func TestAnExpiredProposalNeverEntersTheOrderStore(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")
	pastTTL(svc)

	if err := svc.ExpireProposals(testCtx()); err != nil {
		t.Fatalf("ExpireProposals: %v", err)
	}

	if _, _, err := store.Load(context.Background(), cmd.GetOrderId()); err == nil {
		t.Fatal("expiring a proposal ADMITTED the order — the sweeper created the very thing the " +
			"hold refused to create, and then rejected it")
	}
}

// EXACTLY ONCE, ACROSS TICKS. The sweeper runs every interval forever; without
// the marker it republishes the same rejection on each pass, and the counter
// that exists to make a lost FACT alertable fills with noise.
func TestASecondSweepDoesNotAnnounceTheSameExpiryAgain(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	holdOne(t, svc, store, "user:alice@kanz")
	pastTTL(svc)

	if err := svc.ExpireProposals(testCtx()); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	before := rejectionsFor(fb, ReasonApprovalExpired)
	if err := svc.ExpireProposals(testCtx()); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if got := rejectionsFor(fb, ReasonApprovalExpired); got != before {
		t.Fatalf("ORDER_REJECTED published %d times after two sweeps, want %d — every tick "+
			"re-announces the same dead order for as long as the deployment runs", got, before)
	}
}

// A LIVE PROPOSAL SURVIVES THE SWEEP. The approver still has time, and killing
// the order under them would turn a control into a deadline nobody agreed to.
func TestASweepLeavesALiveProposalAlone(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	if err := svc.ExpireProposals(testCtx()); err != nil {
		t.Fatalf("ExpireProposals: %v", err)
	}

	if fb.last(EventTypeRejected) != nil {
		t.Fatal("a proposal still inside its window was announced as expired")
	}
	p, ok, _ := store.Proposals().Get(context.Background(), cmd.GetOrderId())
	if !ok || !p.ExpiryAnnouncedAt.IsZero() {
		t.Fatalf("a live proposal was marked announced (ok=%v at=%v)", ok, p.ExpiryAnnouncedAt)
	}
}

// AN APPROVED ORDER IS NEVER EXPIRED. It traded. Announcing its proposal's
// deadline afterwards would tell the estate a live order was abandoned.
func TestASweepNeverExpiresAnApprovedOrder(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "user:bob@kanz"))); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}
	pastTTL(svc)

	if err := svc.ExpireProposals(testCtx()); err != nil {
		t.Fatalf("ExpireProposals: %v", err)
	}

	msg := fb.last(EventTypeRejected)
	if msg != nil {
		if rej, ok := msg.(*orderpb.OrderRejected); ok && rej.GetErrorCode() == ReasonApprovalExpired {
			t.Fatal("an APPROVED order was announced as expired — it is live at a venue and the " +
				"estate has just been told it was abandoned")
		}
	}
}
