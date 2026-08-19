package order

// A REFUSED APPROVAL IS VISIBLE ON THE QUEUE THE APPROVER ALREADY READS (#558).
//
// Before this, every refusal in handleApprove returned nil after a WARN. The
// gateway had answered 202 at publish time, no FACT went out, and none could:
// all four CommandOutcomeStatus values are terminal and a refused proposal is
// not, because the order is still pending and somebody else may legitimately
// sign it. The approver's only signal was the order still being on
// ListPendingApprovals — indistinguishable from an order nobody had touched.
//
// The assertions below are therefore in two halves, and BOTH are load-bearing:
//
//  1. the refusal is READABLE — reason, subject and time on the proposal, which
//     ListPendingApprovals renders as state = "REFUSED";
//  2. the proposal is STILL PENDING and STILL CLAIMABLE. Recording must not
//     decide anything, or one person could destroy a colleague's pending
//     decision by attempting their own approval and being turned away. That is
//     the property TestSelfApprovalIsRefusedAndTheProposalStaysPending pins, and
//     nothing here may weaken it.

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/oms/internal/approval"
)

// refusalOn reads back the refusal the OMS recorded for orderID, failing if
// there is no proposal left to read — which is itself the defect in (2).
func refusalOn(t *testing.T, store *MemoryStore, orderID string) OrderProposal {
	t.Helper()
	p, ok, err := store.Proposals().Get(context.Background(), orderID)
	if err != nil || !ok {
		t.Fatalf("the proposal is gone after a refused approval (ok=%v err=%v) — a refusal "+
			"consumed it, and the legitimate approver can no longer act", ok, err)
	}
	return p
}

// assertStillClaimable is half (2), asserted through the store rather than by
// reading a field: the question is not "is approver empty" but "can somebody
// else still sign this order".
func assertStillClaimable(t *testing.T, store *MemoryStore, orderID string) {
	t.Helper()
	pending, err := store.Proposals().Pending(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	var listed bool
	for _, p := range pending {
		if p.ID == orderID {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("order %s left the approval queue after a refusal — the refusal is now the "+
			"silent drop it was filed to end, with the order invisible instead of merely "+
			"unexplained", orderID)
	}
}

// TestASelfApprovalIsRecordedOnTheQueueTheApproverReads is the headline case of
// #558: the refusal an approver was told nothing about.
func TestASelfApprovalIsRecordedOnTheQueueTheApproverReads(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "user:alice@kanz"))); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}

	p := refusalOn(t, store, cmd.GetOrderId())
	if p.RefusalReason != RefusalSelfApproval {
		t.Errorf("refusal reason = %q, want %q — the approver is shown an order that looks "+
			"identical to one nobody has touched, which is the whole of #558",
			p.RefusalReason, RefusalSelfApproval)
	}
	if p.RefusedBy != "user:alice@kanz" {
		t.Errorf("refused_by = %q — the queue can say a refusal happened and not whose, so an "+
			"approver cannot tell their own refused signature from somebody else's", p.RefusedBy)
	}
	if p.RefusedAt.IsZero() {
		t.Error("refused_at is zero — the queue cannot say whether the refusal predates the " +
			"terms currently shown")
	}
	if p.Approver != "" || !p.DecidedAt.IsZero() {
		t.Fatalf("the refusal DECIDED the proposal (approver=%q decided_at=%v) — one person can "+
			"now destroy a colleague's pending decision by attempting their own",
			p.Approver, p.DecidedAt)
	}
	assertStillClaimable(t, store, cmd.GetOrderId())
}

// A RESPELLING OF THE PROPOSER IS THE SAME PERSON, AND MUST BE TOLD SO IN THE
// SAME WORDS. If this reported a different reason from the case above, an
// approver who capitalised a letter in their own subject would be shown a
// refusal that does not describe what happened.
func TestARespellingOfTheProposerIsRecordedAsSelfApproval(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "  User:Alice@Kanz  "))); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}

	p := refusalOn(t, store, cmd.GetOrderId())
	if p.RefusalReason != RefusalSelfApproval {
		t.Errorf("refusal reason = %q, want %q", p.RefusalReason, RefusalSelfApproval)
	}
	assertStillClaimable(t, store, cmd.GetOrderId())
}

// A DIGEST COVERING OTHER TERMS GETS ITS OWN REASON, and that distinction is the
// point of a code rather than a sentence: "find another approver" and "the terms
// moved, re-propose the order" are different actions, and an approver shown one
// label for both would take the wrong one.
func TestADigestCoveringOtherTermsIsRecordedAsAPayloadChange(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))
	cmd := holdOne(t, svc, store, "user:alice@kanz")

	appr := approvalOf(t, cmd, "user:bob@kanz")
	appr.Digest = "sha256:not-the-terms-that-were-proposed"
	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, appr)); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}

	p := refusalOn(t, store, cmd.GetOrderId())
	if p.RefusalReason != RefusalPayloadChange {
		t.Errorf("refusal reason = %q, want %q — a refusal the approver cannot act on correctly "+
			"is barely better than one they were never told about", p.RefusalReason, RefusalPayloadChange)
	}
	if p.RefusedBy != "user:bob@kanz" {
		t.Errorf("refused_by = %q, want the approver whose signature was refused", p.RefusedBy)
	}
	assertStillClaimable(t, store, cmd.GetOrderId())
}

// AN EXPIRED PROPOSAL IS RECORDED AND IS NOT ON THE QUEUE, and that combination
// is deliberate rather than a gap. Pending excludes expired work — offering a
// signature on an order that can no longer be released would be worse than
// silence — and the approver's answer for expiry is the terminal ORDER_REJECTED
// #547's sweeper publishes. The record is the operator's evidence that somebody
// tried after the deadline.
func TestAnExpiredProposalRecordsItsRefusalButIsNotOfferedAsWork(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))

	// Held long enough ago that its TTL has passed against the real clock the
	// service runs on.
	born := time.Now().UTC().Add(-48 * time.Hour)
	held := heldOrder(t, "o-expired-refusal", "user:alice@kanz", born)
	if err := store.Proposals().Put(context.Background(), held, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, held.Command, "user:bob@kanz"))); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}

	p := refusalOn(t, store, "o-expired-refusal")
	if p.RefusalReason != RefusalExpired {
		t.Errorf("refusal reason = %q, want %q", p.RefusalReason, RefusalExpired)
	}
	if p.Approver != "" {
		t.Fatalf("an expired proposal was DECIDED by a refusal (approver=%q)", p.Approver)
	}
	pending, err := store.Proposals().Pending(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	for _, q := range pending {
		if q.ID == "o-expired-refusal" {
			t.Fatal("an EXPIRED proposal is offered on the approval queue — the queue is " +
				"inviting a signature on an order that can never be released, which is a worse " +
				"answer than the silence #558 replaced")
		}
	}
}

// AN APPROVAL FOR AN ORDER NOBODY HELD MUST NOT CREATE A PROPOSAL TO CARRY ITS
// OWN REFUSAL. This is the branch with nowhere to write, and inventing somewhere
// would let a subject holding only the approve authority originate a held order
// — precisely the person who must not be able to.
func TestAnApprovalForAnUnheldOrderCreatesNothingToRecordAgainst(t *testing.T) {
	fb := &fakeBus{}
	svc, _, store := armedService(t, fb, dualRat("1000"))

	cmd := largeOrderFrom("user:alice@kanz")
	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "user:bob@kanz"))); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}

	if _, ok, err := store.Proposals().Get(context.Background(), cmd.GetOrderId()); err != nil || ok {
		t.Fatalf("an approval created a proposal so it had somewhere to record its refusal "+
			"(ok=%v err=%v) — the approve authority can now originate held orders", ok, err)
	}
	pending, err := store.Proposals().Pending(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("the approval queue grew from an approval for an order nobody held: %v",
			ids(pending))
	}
}

// A REFUSAL THAT CANNOT BE WRITTEN MUST NACK, NOT ACK.
//
// Returning nil when the store refuses the write would ack a delivery whose only
// trace is a log line the approver cannot read — the exact silence this issue was
// filed for, restored by one unchecked error. The nack redelivers, which
// re-refuses and re-records: the columns are overwritten with the same values.
func TestARefusalThatCannotBeRecordedIsRedeliveredRatherThanAcked(t *testing.T) {
	fb := &fakeBus{}
	reg := prometheus.NewRegistry()
	g, err := approval.NewGate(true, dualRat("1000"), nil, reg)
	if err != nil {
		t.Fatalf("NewGate armed: %v", err)
	}
	mem := NewMemoryStore()
	// THE ONLY SUBSTITUTION IS THE ONE WRITE. Everything else is the real store,
	// so this exercises the real handleApprove path right up to the point the
	// refusal has to be recorded.
	store := &storeThatCannotRecordRefusals{MemoryStore: mem}
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, nil, nil, nil, WithDualControl(g))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	cmd := holdOne(t, svc, mem, "user:alice@kanz")

	if err := svc.Handle(testCtx(), approveEnv(), mustMarshal(t, approvalOf(t, cmd, "user:alice@kanz"))); err == nil {
		t.Fatal("a refusal whose record could not be written was ACKED — the approver is told " +
			"nothing, the delivery is gone, and the only evidence is a WARN nobody reads")
	}
}

type storeThatCannotRecordRefusals struct{ *MemoryStore }

func (s *storeThatCannotRecordRefusals) Proposals() ProposalStore {
	return refusingToRecord{ProposalStore: s.MemoryStore.Proposals()}
}

// refusingToRecord is a proposal store whose RecordRefusal always fails.
type refusingToRecord struct{ ProposalStore }

func (refusingToRecord) RecordRefusal(context.Context, string, string, string, time.Time) (bool, error) {
	return false, context.DeadlineExceeded
}
