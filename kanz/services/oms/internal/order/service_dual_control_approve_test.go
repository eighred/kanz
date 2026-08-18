package order

// THE RELEASE (#539) — a held order that a second person signs becomes a live order.
//
// service_dual_control_hold_test.go proves an order above the threshold does NOT
// become an admitted order. That half shipped alone, which made the control a
// one-way door: the OMS could hold an order and nothing anywhere could release
// it, so every held order sat until it expired and died in silence. These prove
// the other half.
//
// The load-bearing assertion in the headline case is the mirror of the hold's:
// the hold asserts ABSENCE from the order store, this asserts PRESENCE. And in
// every refusal case the assertion is that the proposal is STILL PENDING — a
// refused approval that consumed the proposal would let one person destroy a
// colleague's pending decision by trying to approve their own order.

import (
	"context"
	"errors"
	"testing"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/services/oms/internal/approval"
)

func approveEnv() *envelopepb.Envelope { return &envelopepb.Envelope{EventType: SubjectApprove} }

// approvalOf builds the command a second subject sends to release a held order.
// The digest is re-derived from the terms rather than read back off the
// proposal, which is what an approver's client does — trusting the stored
// digest would make the signature cover whatever the store happens to say.
func approvalOf(t *testing.T, cmd *orderpb.SubmitOrder, approver string) *orderpb.ApproveOrder {
	t.Helper()
	digest, err := approval.TermsOfSubmit(cmd).Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	return &orderpb.ApproveOrder{
		Metadata: &commandpb.CommandMetadata{Issuer: approver, TargetId: cmd.GetOrderId()},
		OrderId:  cmd.GetOrderId(),
		Digest:   digest,
	}
}

// holdOne submits a large order into an armed service and returns it, having
// asserted it really was held. Every case below starts from a held order, and a
// setup that silently admitted instead would make the approvals below prove
// nothing.
func holdOne(t *testing.T, svc *Service, store *MemoryStore, proposer string) *orderpb.SubmitOrder {
	t.Helper()
	cmd := largeOrderFrom(proposer)
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle(submit): %v", err)
	}
	if _, _, err := store.Load(context.Background(), cmd.GetOrderId()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("setup: the order was ADMITTED rather than held (err=%v)", err)
	}
	return cmd
}

// THE TEST THIS WHOLE CHANGE EXISTS TO MAKE PASS.
//
// A different authenticated subject approves, and the order becomes real: in the
// store, announced as ORDER_ACCEPTED, under the SAME order_id it was proposed
// under. The proto is explicit that the id does not change — a consumer must not
// treat the later ORDER_ACCEPTED as a duplicate, and an approver must not have
// to explain to anyone why the order they signed has a different identity.
func TestAnApprovedOrderBecomesALiveOrder(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	appr := approvalOf(t, cmd, "user:bob@kanz")
	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, appr)); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}

	st, _, err := store.Load(context.Background(), cmd.GetOrderId())
	if err != nil {
		t.Fatalf("the approved order is NOT in the order store (err=%v) — a second person signed it "+
			"and nothing happened, which is the one-way door #539 exists to close", err)
	}
	if st.GetOrderId() != cmd.GetOrderId() {
		t.Errorf("admitted under order_id %q, want %q — the proposal and the order must be the "+
			"same aggregate or nobody can prove the order that traded is the order that was signed",
			st.GetOrderId(), cmd.GetOrderId())
	}
	if fb.last(EventTypeAccepted) == nil {
		t.Fatalf("no ORDER_ACCEPTED was published (published: %v) — the order is live in the store "+
			"and the estate never heard, so the blotter and the projector both stay blind", fb.types())
	}
}

// THE PROPOSAL IS CONSUMED, AND BOTH NAMES ARE ON IT. The row is the durable
// evidence an examiner reads; an approved order whose proposal still says
// "pending" would let the same signature be spent twice.
func TestApprovingRecordsBothIdentitiesOnTheProposal(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "user:bob@kanz"))); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}

	p, ok, err := store.Proposals().Get(context.Background(), cmd.GetOrderId())
	if err != nil || !ok {
		t.Fatalf("proposal missing after approval (ok=%v err=%v)", ok, err)
	}
	if p.Approver != "user:bob@kanz" {
		t.Errorf("approver = %q, want the authenticated second subject — the trail must name who "+
			"countersigned, not merely that somebody did", p.Approver)
	}
	if p.Proposer != "user:alice@kanz" {
		t.Errorf("proposer = %q, want the original submitter", p.Proposer)
	}
	if p.DecidedAt.IsZero() {
		t.Error("decided_at is zero on a decided proposal — the queue cannot tell a signed proposal " +
			"from one still awaiting a signature")
	}
}

// AND IT IS COUNTED AS DUAL-SIGNED. kanz_oms_order_signatures_total is the
// number the arming decision rests on: how much of the order flow went through
// on one signature. An approved order counted as single_signed would make the
// control look ineffective exactly as it started working.
func TestAnApprovedOrderIsCountedAsDualSigned(t *testing.T) {
	fb := &fakeBus{}
	svc, reg, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "user:bob@kanz"))); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}

	if got := signatures(t, reg, approval.DualSigned, string(approval.PostureAtOrAbove)); got != 1 {
		t.Errorf("signatures{signatures=%q} = %v, want 1 — an order two people signed is being "+
			"counted as one, so the metric understates the control precisely when it works",
			approval.DualSigned, got)
	}
	if got := signatures(t, reg, approval.SingleSigned, string(approval.PostureAtOrAbove)); got != 0 {
		t.Errorf("signatures{signatures=%q} = %v, want 0", approval.SingleSigned, got)
	}
}

// SELF-APPROVAL IS REFUSED, AND THE PROPOSAL SURVIVES.
//
// The survival half is not a detail. If a rejected self-approval consumed the
// proposal, one person could destroy a colleague's pending decision simply by
// trying to approve their own order — a denial of service delivered through the
// control. datamaster settled this ordering on the override path: the rule is
// checked BEFORE the claim.
func TestSelfApprovalIsRefusedAndTheProposalStaysPending(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "user:alice@kanz"))); err != nil {
		t.Fatalf("Handle(approve) returned a transient error, want a recorded refusal: %v", err)
	}

	if _, _, err := store.Load(context.Background(), cmd.GetOrderId()); !errors.Is(err, ErrNotFound) {
		t.Fatal("the proposer approved their own order and it TRADED — the trail shows two events " +
			"and one person held both, which is precisely what maker-checker exists to prevent")
	}
	p, ok, _ := store.Proposals().Get(context.Background(), cmd.GetOrderId())
	if !ok || p.Approver != "" {
		t.Fatalf("the proposal was consumed by a refused self-approval (ok=%v approver=%q) — one "+
			"person can now destroy a colleague's pending decision by attempting their own",
			ok, p.Approver)
	}
}

// ONE PERSON SPELLED TWO WAYS IS STILL ONE PERSON. A case-sensitive comparison
// would let the proposer approve by capitalising a letter, and the audit trail
// would then show two distinct actors — satisfied, in exactly the record an
// examiner would check.
func TestSelfApprovalByRespellingIsRefused(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "  User:Alice@Kanz  "))); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}

	if _, _, err := store.Load(context.Background(), cmd.GetOrderId()); !errors.Is(err, ErrNotFound) {
		t.Fatal("a respelling of the proposer approved their own order and it traded")
	}
}

// AN APPROVAL THAT COVERS DIFFERENT TERMS IS REFUSED. This is the attack the
// digest exists for: propose a defensible order, collect the signature, apply a
// different one. The approver signs the VALUE, not the request.
func TestAnApprovalCoveringOtherTermsIsRefused(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	appr := approvalOf(t, cmd, "user:bob@kanz")
	appr.Digest = "sha256:not-the-terms-that-were-proposed"
	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, appr)); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}

	if _, _, err := store.Load(context.Background(), cmd.GetOrderId()); !errors.Is(err, ErrNotFound) {
		t.Fatal("an approval whose digest covers other terms admitted the order — the signature " +
			"covers the id and not the value, so the order that traded is not the order signed for")
	}
	p, ok, _ := store.Proposals().Get(context.Background(), cmd.GetOrderId())
	if !ok || p.Approver != "" {
		t.Error("a mismatched digest consumed the proposal — the legitimate approver can no longer act")
	}
}

// AN EXPIRED PROPOSAL CANNOT BE APPROVED. Without the deadline an approval
// collected today applies against next quarter's book.
func TestApprovingAnExpiredProposalIsRefused(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	// Age the clock past the proposal's TTL rather than mutating the row: the
	// deadline is a property of time passing, and a test that rewrote expires_at
	// would prove the field is checked rather than that expiry is enforced.
	svc.now = func() time.Time { return time.Now().UTC().Add(dualcontrol.DefaultTTL + time.Hour) }

	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "user:bob@kanz"))); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}

	if _, _, err := store.Load(context.Background(), cmd.GetOrderId()); !errors.Is(err, ErrNotFound) {
		t.Fatal("an expired proposal was approved and traded — a proposal nobody acted on for a day " +
			"is not a decision that was made")
	}
}

// A SECOND APPROVAL DOES NOT ADMIT THE ORDER TWICE. Claim is the serialisation
// point and the OMS ships two replicas, so this is a redelivery and a race at
// once. Admitting twice means two deliveries reaching a live venue.
func TestASecondApprovalDoesNotAdmitTheOrderAgain(t *testing.T) {
	fb := &fakeBus{}
	svc, reg, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	appr := approvalOf(t, cmd, "user:bob@kanz")
	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, appr)); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, appr)); err != nil {
		t.Fatalf("second approve returned an error rather than acking a settled decision: %v", err)
	}

	if got := signatures(t, reg, approval.DualSigned, string(approval.PostureAtOrAbove)); got != 1 {
		t.Errorf("dual_signed = %v after two deliveries of one approval, want 1 — the order was "+
			"admitted twice, which on this path is two deliveries to a live venue", got)
	}
}

// APPROVING AN ORDER NOBODY PROPOSED IS REFUSED RATHER THAN CREATING ONE. An
// approval is a release, never an origin: admitting here would let a single
// subject holding only the approve capability originate an order outright.
func TestApprovingAnUnknownOrderAdmitsNothing(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))

	appr := &orderpb.ApproveOrder{
		Metadata: &commandpb.CommandMetadata{Issuer: "user:bob@kanz", TargetId: "ord-never-proposed"},
		OrderId:  "ord-never-proposed",
		Digest:   "sha256:whatever",
	}
	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, appr)); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}

	if _, _, err := store.Load(context.Background(), "ord-never-proposed"); !errors.Is(err, ErrNotFound) {
		t.Fatal("an approval CREATED an order that nobody submitted — the approve capability is " +
			"now a submit capability, held by the one person who must not have it")
	}
}
