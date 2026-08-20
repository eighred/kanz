package order

// THE APPROVAL IS A FACT (#410, acceptance clause (d)).
//
// service_dual_control_approve_test.go already asserts that the second signature
// lands on the PROPOSAL ROW. That is what shipped, and it is not what this
// issue's acceptance asked for. A ROW IS NOT A FACT: services/audit folds bus
// FACTs into the signed store an examiner reads and cannot see another service's
// tables, so an approval that never becomes an envelope is invisible to the
// trail no matter what order_proposals holds. The trail could show that an order
// was approved and could not say by whom — on the one act where the capital
// actually moves.
//
// So every assertion here reads a PUBLISHED EVENT rather than a store. The
// broker half — that the envelope is one a real JetStream accepts and that both
// names survive the round trip — is approval_fact_integration_test.go, because
// fakeBus does not run bus.Validate and a green suite using it is not broker
// proof.

import (
	"context"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/outbox"
)

// approvedFact returns the ORDER_APPROVED payload the service published, or nil.
func approvedFact(t *testing.T, fb *fakeBus) *orderpb.OrderApproved {
	t.Helper()
	msg := fb.last(EventTypeApproved)
	if msg == nil {
		return nil
	}
	got, ok := msg.(*orderpb.OrderApproved)
	if !ok {
		t.Fatalf("%s carried a %T, not an OrderApproved", EventTypeApproved, msg)
	}
	return got
}

// THE TEST THIS WHOLE CHANGE EXISTS TO MAKE PASS.
//
// It asserts the PUBLISHED ENVELOPE, not the proposal row — the distinction is
// the defect. Both names, on one FACT, on the bus.
func TestApprovingPublishesAFactNamingBothIdentities(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "user:bob@kanz"))); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}

	got := approvedFact(t, fb)
	if got == nil {
		t.Fatalf("no %s was published (published: %v) — a second person signed a live order and "+
			"the only record of it is a row services/audit cannot read, which is exactly the "+
			"state #410's clause (d) was filed against", EventTypeApproved, fb.types())
	}
	if got.GetProposer() != "user:alice@kanz" {
		t.Errorf("proposer = %q, want user:alice@kanz — a FACT naming only the approver is "+
			"compatible with one person holding both signatures", got.GetProposer())
	}
	if got.GetApprover() != "user:bob@kanz" {
		t.Errorf("approver = %q, want user:bob@kanz — this is the name that did not exist "+
			"anywhere on the bus before, and the whole point of the FACT", got.GetApprover())
	}
	if got.GetOrderId() != cmd.GetOrderId() {
		t.Errorf("order_id = %q, want %q — an approval that cannot be tied to the order it "+
			"released is not evidence of anything", got.GetOrderId(), cmd.GetOrderId())
	}
	if got.GetAct() != string(dualcontrol.ActOrderSubmission) {
		t.Errorf("act = %q, want %q — the act is what stops an approval collected for a pricing "+
			"override being read as one for an order", got.GetAct(), dualcontrol.ActOrderSubmission)
	}
	if got.GetApprovedAt().AsTime().IsZero() {
		t.Error("approved_at is unset — an examiner can see that somebody signed but not when, " +
			"which is the half that dates the decision against the book")
	}
}

// THE DIGEST TIES THE TWO FACTS TOGETHER. ORDER_PENDING_APPROVAL announced what
// was proposed; this announces that it was signed. If the digests differ, a
// consumer holding both cannot prove the thing signed is the thing proposed —
// and the approver's signature covers a payload nobody can identify.
func TestTheApprovalFactCarriesTheDigestTheHoldAnnounced(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	held, ok := fb.last(EventTypePendingApproval).(*orderpb.OrderPendingApproval)
	if !ok || held == nil {
		t.Fatalf("setup: no %s was published (published: %v)", EventTypePendingApproval, fb.types())
	}
	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "user:bob@kanz"))); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}

	got := approvedFact(t, fb)
	if got == nil {
		t.Fatalf("no %s was published (published: %v)", EventTypeApproved, fb.types())
	}
	if got.GetDigest() == "" || got.GetDigest() != held.GetDigest() {
		t.Errorf("the approval's digest is %q and the hold announced %q — a consumer holding both "+
			"cannot prove the order that traded is the order that was signed",
			got.GetDigest(), held.GetDigest())
	}
}

// NON-VACUITY, AND IT IS THE SHAPE OF A BROKEN CONTROL. A build that published
// ORDER_APPROVED unconditionally would satisfy every assertion above while the
// trail recorded approvals nobody gave. A refused approval must announce
// nothing — and must leave the order held.
func TestARefusedApprovalPublishesNoApprovalFact(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	// The proposer signing their own order. dualcontrol refuses it, the store
	// refuses it again, and the database refuses it a third time.
	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "user:alice@kanz"))); err != nil {
		t.Fatalf("Handle(self-approve): %v", err)
	}

	if got := approvedFact(t, fb); got != nil {
		t.Fatalf("a SELF-APPROVAL was announced as an approval carrying %q/%q — the audit trail "+
			"now shows four-eyes on an order one person signed alone, which is the single most "+
			"valuable row to forge", got.GetProposer(), got.GetApprover())
	}
	p, ok, err := store.Proposals().Get(context.Background(), cmd.GetOrderId())
	if err != nil || !ok {
		t.Fatalf("proposal missing after a refused approval (ok=%v err=%v)", ok, err)
	}
	if p.Approver != "" {
		t.Errorf("approver = %q on a refused approval", p.Approver)
	}
}

// A CLAIM THAT ANNOUNCES NOTHING IS REFUSED, LOUDLY.
//
// This is the "nothing configured" half of the rule. Claim's whole reason for
// taking announce is that the second signature has to leave the process; a
// caller that passes none would put it in a table and nowhere else, and from
// inside the OMS that is indistinguishable from a healthy claim. So it fails
// rather than defaulting, and the proposal stays pending — an approval that
// could not be recorded as a FACT must not be recorded at all.
func TestAClaimThatWouldAnnounceNothingIsRefused(t *testing.T) {
	s := newMemoryProposals()
	ctx := context.Background()
	if err := s.Put(ctx, heldOrder(t, "o-silent", "user:alice@kanz", t0), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, announce := range [][]outbox.Record{nil, {}} {
		won, err := s.Claim(ctx, "o-silent", "user:bob@kanz", t0.Add(time.Minute), announce)
		if err == nil {
			t.Fatalf("Claim accepted an empty announce (won=%v) — the second signature would be "+
				"durable in order_proposals and invisible to services/audit, which is the defect "+
				"#410's clause (d) names", won)
		}
		if won {
			t.Fatal("Claim reported a win it refused")
		}
	}

	got, ok, err := s.Get(ctx, "o-silent")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Approver != "" {
		t.Errorf("approver = %q — a refused claim decided the proposal anyway, so the order is "+
			"released with nothing announcing who released it", got.Approver)
	}
}

// AND SO IS ONE FROM NOBODY. An empty subject makes the self-approval check pass
// vacuously — every proposer differs from "" — so this is the same defect class
// #444 fixed on the override path, arriving at the store instead of the handler.
func TestAClaimFromNoAuthenticatedSubjectIsRefused(t *testing.T) {
	s := newMemoryProposals()
	ctx := context.Background()
	if err := s.Put(ctx, heldOrder(t, "o-anon", "user:alice@kanz", t0), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	won, err := s.Claim(ctx, "o-anon", "  ", t0.Add(time.Minute),
		approvalAnnounce(t, "o-anon", t0.Add(time.Minute)))
	if err == nil || won {
		t.Fatalf("Claim accepted an unauthenticated approver (won=%v err=%v) — the row would read "+
			"to an auditor as a countersigned order signed by nobody", won, err)
	}
}

// THE SIGNATURE AND ITS ANNOUNCEMENT COMMIT TOGETHER in the memory store too,
// which is the seam every service-level test in this package runs on. A seam
// that recorded the claim and dropped the FACT would certify behaviour the
// Postgres store does not have — the divergence fakeBus already taught this
// repository to distrust.
func TestAClaimWhoseFactCannotBeEnqueuedRecordsNoSignature(t *testing.T) {
	q := outbox.NewMemory()
	s := NewMemoryProposals(q)
	ctx := context.Background()
	if err := s.Put(ctx, heldOrder(t, "o-rollback", "user:alice@kanz", t0), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// A record with no tenant is one outbox.Enqueue and bus.Validate both refuse,
	// so it can never be published — the shape that crash-looped the OMS when a
	// publish happened outside an inbound delivery.
	bad := outbox.Record{
		Subject: EventTypeApproved, EventType: EventTypeApproved,
		Domain: Domain, PartitionKey: "o-rollback", EventTime: t0,
		PayloadSchemaRef: "order.v1.OrderApproved:1", Payload: []byte{},
		// TenantID deliberately empty.
	}
	if won, err := s.Claim(ctx, "o-rollback", "user:bob@kanz", t0.Add(time.Minute),
		[]outbox.Record{bad}); err == nil || won {
		t.Fatalf("Claim accepted a FACT that can never be published (won=%v err=%v)", won, err)
	}
	got, ok, err := s.Get(ctx, "o-rollback")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Approver != "" {
		t.Errorf("approver = %q — the signature survived a failed enqueue, so the two are not "+
			"one write and the order is released with no announcement behind it", got.Approver)
	}
}
