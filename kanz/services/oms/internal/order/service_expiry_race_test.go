package order

import (
	"context"
	"errors"
	"testing"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"

	"github.com/eighred/kanz/internal/dualcontrol"
)

// ONE COMMAND MUST NOT GET TWO TERMINAL ANSWERS (#796).
//
// TestApprovingAnExpiredProposalIsRefused already covers the ordinary late
// approval: the clock has passed the deadline, dualcontrol.Approve refuses in
// Go, and the order never exists. THIS is the other one — the approval whose Go
// check PASSES and which the sweeper beats to the row.
//
// The window needs no clock skew and no second process. handleApprove captures
// `now` before the Proposals().Get round trip, Approve checks the deadline
// against that captured value, and the Claim runs later still, while
// ExpireProposals runs on its own ticker in the composition root. Reproduced
// here deterministically by moving the service clock forward for the sweep and
// back for the approval — which is what "the approval was already in flight when
// the ticker fired" looks like from the row's point of view.
//
// WHAT IT COSTS WHEN IT IS WRONG: the estate receives a terminal ORDER_REJECTED
// / DUAL_CONTROL_EXPIRED and then ORDER_APPROVED, ORDER_ACCEPTED and fills for
// the same order_id. Capital moves on a maker-checker approval the platform had
// already declared dead, the audit trail records the order as having died on its
// deadline, and every downstream projection that folds terminality is
// reconstructing a history the platform did not have.

// sweepThenApprove holds an order, announces its expiry through the real sweeper
// with the clock moved forward, then restores the clock and approves. It returns
// the order id the two arms below assert against.
func sweepThenApprove(t *testing.T, svc *Service, store *MemoryStore) string {
	t.Helper()
	cmd := holdOne(t, svc, store, "user:alice@kanz")
	realNow := svc.now

	// THE TICKER FIRES. Moved forward only for the sweep — the announcement is a
	// property of the deadline having passed, and the sweeper re-checks it in the
	// UPDATE rather than trusting the caller.
	svc.now = func() time.Time { return time.Now().UTC().Add(dualcontrol.DefaultTTL + time.Hour) }
	if err := svc.ExpireProposals(testCtx()); err != nil {
		t.Fatalf("ExpireProposals: %v", err)
	}
	p, ok, err := store.Proposals().Get(context.Background(), cmd.GetOrderId())
	if err != nil || !ok {
		t.Fatalf("proposal gone after the sweep: %v (found=%v)", err, ok)
	}
	if p.ExpiryAnnouncedAt.IsZero() {
		t.Fatal("the sweeper announced nothing, so the race this test needs was never set up and " +
			"the assertions below would pass on a service that simply never expires anything")
	}
	if p.Approver != "" {
		t.Fatalf("the sweeper set approver = %q — it must not decide the proposal, and if it did "+
			"the old `approver = ''` predicate would already have refused the claim", p.Approver)
	}

	// THE APPROVAL, WITH THE CLOCK BACK WHERE IT WAS. dualcontrol.Approve sees a
	// live proposal and passes; only the store can refuse from here.
	svc.now = realNow
	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "user:bob@kanz"))); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}
	return cmd.GetOrderId()
}

// THE ORDER MUST NOT COME INTO EXISTENCE.
func TestAnApprovalThatLostToTheExpirySweepAdmitsNothing(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))

	orderID := sweepThenApprove(t, svc, store)

	if _, _, err := store.Load(context.Background(), orderID); !errors.Is(err, ErrNotFound) {
		t.Fatal("an order whose expiry the estate was ALREADY TOLD about was admitted and is now " +
			"live in the book. The platform published a terminal ORDER_REJECTED for it and then " +
			"traded it: two terminal answers to one command, on a second signature that arrived " +
			"after the decision was declared dead.")
	}
	if fb.last(EventTypeAccepted) != nil {
		t.Error("ORDER_ACCEPTED was published for an order already announced REJECTED — every " +
			"consumer that folds terminality now holds a history the platform did not have")
	}
	if fb.last(EventTypeApproved) != nil {
		t.Error("ORDER_APPROVED was published after the terminal rejection. The approval FACT is " +
			"what the audit trail reads as evidence two people released this order, and it must " +
			"not exist for an order nobody released")
	}
}

// AND THE APPROVER IS TOLD THE TRUTH ABOUT WHY.
//
// "lost_the_claim" and "expired" demand OPPOSITE responses: losing to a
// signature means the order is live and there is nothing to do, while losing to
// the deadline means nobody signed it in time and it must be PROPOSED AGAIN.
// Reporting the second as the first sends the approver to find a co-signer who
// does not exist — the shape #558 spent an issue on.
func TestAnApprovalThatLostToTheExpirySweepIsToldItExpired(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))

	orderID := sweepThenApprove(t, svc, store)

	p, ok, err := store.Proposals().Get(context.Background(), orderID)
	if err != nil || !ok {
		t.Fatalf("proposal gone: %v (found=%v)", err, ok)
	}
	if p.RefusalReason != RefusalExpired {
		t.Fatalf("the approver is told %q, want %q. The proposal is still pending and its "+
			"approver is empty, so %q would send them to look for the signature that beat "+
			"them — there isn't one. The deadline beat them, and re-proposing is the action.",
			p.RefusalReason, RefusalExpired, RefusalLostTheClaim)
	}
	if p.RefusedBy != "user:bob@kanz" {
		t.Errorf("refused_by = %q, want the approver who was turned away", p.RefusedBy)
	}
}

// NON-VACUITY FOR THE REASON. An approval that loses to a real SIGNATURE must
// still be told "lost_the_claim" — otherwise the test above would pass on a
// service that reported "expired" for every refused claim, which is the same
// defect pointed the other way.
func TestAnApprovalThatLostToASignatureIsToldSo(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	// Bob signs it for real. The proposal is decided, and nothing has expired.
	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "user:bob@kanz"))); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}
	if _, _, err := store.Load(context.Background(), cmd.GetOrderId()); err != nil {
		t.Fatalf("setup: the first approval did not admit the order: %v", err)
	}

	// Carol arrives second.
	carol := approvalOf(t, cmd, "user:carol@kanz")
	carol.Metadata = &commandpb.CommandMetadata{Issuer: "user:carol@kanz", TargetId: cmd.GetOrderId()}
	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, carol)); err != nil {
		t.Fatalf("Handle(approve) second: %v", err)
	}

	p, ok, err := store.Proposals().Get(context.Background(), cmd.GetOrderId())
	if err != nil || !ok {
		t.Fatalf("proposal gone: %v (found=%v)", err, ok)
	}
	// A DECIDED proposal's row is the OMS's only durable evidence two people
	// signed, so RecordRefusal declines to touch it — the reason stays empty and
	// the approver's answer is the order's absence from the queue. What must NOT
	// happen is the row being labelled "expired", which nothing here is.
	if p.RefusalReason == RefusalExpired {
		t.Fatalf("an approval that lost to a real signature was told %q — nothing expired, and the "+
			"approver would re-propose an order that is already live in the book", RefusalExpired)
	}
	if p.Approver != "user:bob@kanz" {
		t.Errorf("approver = %q, want bob — the winning signature must be what the row records", p.Approver)
	}
}
