package order

// THIS ACT'S half of the ProposalStore contract, against the in-process
// implementation.
//
// EVERY TEST HERE IS ALSO RUN AGAINST POSTGRES in proposals_postgres_test.go via
// runProposalContract, and that is deliberate: the memory store is the seam every
// service-level test in this package runs on, so a seam that accepted what
// Postgres refuses would certify behaviour production does not have. The three
// properties that CANNOT be shown here — the CHECK constraint, RLS, and two
// concurrent Claims across connections — have their own gated tests there.
//
// WHAT IS NOT HERE IS IN THE SHARED CONTRACT (#562). Round-tripping the
// dual-control record, refusing a duplicate, electing exactly one claimant,
// refusing a self-approval and a malformed record, and keeping an expired
// proposal discoverable are properties EVERY act must have, so they live in
// internal/dualcontrol/proposalstore/proposalstoretest and run against all four
// shipped backends — see proposals_contract_test.go. Restating them here would
// be the second answer this whole promotion exists to remove.

import (
	"context"
	"strings"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/services/oms/internal/approval"
)

// heldOrder builds a well-formed proposal for orderID proposed by proposer.
func heldOrder(t *testing.T, orderID, proposer string, at time.Time) OrderProposal {
	t.Helper()
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.OrderId = orderID
	base, err := approval.Propose(orderID, approval.TermsOfSubmit(cmd), proposer, at, dualcontrol.DefaultTTL)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	return OrderProposal{Proposal: base, Command: cmd, PortfolioID: cmd.GetPortfolioId()}
}

func newMemoryProposals() *MemoryProposals { return NewMemoryProposals(outbox.NewMemory()) }

// runProposalContract is every property both implementations must have. It takes
// a factory so the Postgres suite can run the identical assertions.
func runProposalContract(t *testing.T, newStore func(t *testing.T) ProposalStore) {
	t.Helper()

	t.Run("a held order can be read back with the command it holds", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		p := heldOrder(t, "o-read", "user:alice@kanz", t0)
		if err := s.Put(ctx, p, nil); err != nil {
			t.Fatalf("Put: %v", err)
		}
		got, ok, err := s.Get(ctx, "o-read")
		if err != nil || !ok {
			t.Fatalf("Get: ok=%v err=%v", ok, err)
		}
		if got.Proposer != p.Proposer || got.Digest != p.Digest || got.Act != p.Act {
			t.Errorf("round trip lost the dual-control record: %+v", got.Proposal)
		}
		// THE COMMAND IS THE POINT. A proposal that cannot produce the order it
		// holds cannot be approved into anything.
		if got.Command.GetOrderId() != "o-read" ||
			got.Command.GetOrderType() != orderpb.OrderType_ORDER_TYPE_LIMIT ||
			got.Command.GetLimitPrice().GetCoefficient() != p.Command.GetLimitPrice().GetCoefficient() {
			t.Errorf("round trip lost the command: %v", got.Command)
		}
		// The digest must still describe the command that came back, or the
		// approval would fail as a payload change against a payload nobody edited.
		want, err := approval.TermsOfSubmit(got.Command).Digest()
		if err != nil {
			t.Fatalf("Digest: %v", err)
		}
		if want != got.Digest {
			t.Errorf("the stored command no longer hashes to the stored digest (%q vs %q) — the "+
				"store re-encoded something the signature covers", want, got.Digest)
		}
	})

	// THAT EXACTLY ONE CALLER WINS IS THE SHARED CONTRACT'S. What is this act's
	// alone is what the winning claim WRITES: the row keeps its approver and the
	// time, because an admitted order carries the order and not who approved it,
	// so this row is the OMS's only durable record that two people signed.
	t.Run("a claim records the second signature on the row", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		if err := s.Put(ctx, heldOrder(t, "o-claim", "user:alice@kanz", t0), nil); err != nil {
			t.Fatalf("Put: %v", err)
		}
		won, err := s.Claim(ctx, "o-claim", "user:bob@kanz", t0.Add(time.Minute))
		if err != nil || !won {
			t.Fatalf("Claim: won=%v err=%v", won, err)
		}
		got, ok, err := s.Get(ctx, "o-claim")
		if err != nil || !ok {
			t.Fatalf("Get after claim: ok=%v err=%v", ok, err)
		}
		if got.Approver != "user:bob@kanz" {
			t.Errorf("approver = %q — this row is the OMS's only durable record that two people "+
				"signed, so an empty one means the audit trail cannot answer who approved it",
				got.Approver)
		}
		if got.DecidedAt.IsZero() {
			t.Error("decided_at is unset on a claimed proposal — an auditor can see that somebody " +
				"signed but not when, which is the half that dates the decision against the book")
		}
	})

	// THE MALFORMED RECORD IS THE SHARED CONTRACT'S (no proposer, no digest, born
	// expired, and the rest). These two are what only an order proposal can be
	// wrong about.
	t.Run("a proposal that could not be admitted is refused rather than stored", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		base := heldOrder(t, "o-bad", "user:alice@kanz", t0)

		noCommand := base
		noCommand.Command = nil
		if err := s.Put(ctx, noCommand, nil); err == nil {
			t.Error("a proposal with no command was stored — approving it could admit nothing, so " +
				"the approver signs for an order that does not exist")
		}

		otherOrder := base
		otherOrder.Command = proto.Clone(base.Command).(*orderpb.SubmitOrder)
		otherOrder.Command.OrderId = "o-somebody-else"
		if err := s.Put(ctx, otherOrder, nil); err == nil {
			t.Error("a proposal keyed on one order was stored holding the command for another — " +
				"the approval would release an order nobody was shown")
		}
	})

	// EXACTLY ONCE, AND THE STORE IS WHAT DECIDES IT. The OMS ships replicas: 2, so
	// two sweepers see the same expired row on the same tick. A second
	// announcement is a second ORDER_REJECTED for one order.
	t.Run("an expiry announces exactly once", func(t *testing.T) {
		s := newStore(t)
		born := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		p := heldOrder(t, "o-once", "user:alice@kanz", born)
		if err := s.Put(context.Background(), p, nil); err != nil {
			t.Fatalf("Put: %v", err)
		}
		afterTTL := p.ExpiresAt.Add(time.Minute)

		first, err := s.AnnounceExpiry(context.Background(), "o-once", afterTTL, nil)
		if err != nil || !first {
			t.Fatalf("first AnnounceExpiry: ok=%v err=%v — nothing announced the expiry", first, err)
		}
		second, err := s.AnnounceExpiry(context.Background(), "o-once", afterTTL, nil)
		if err != nil {
			t.Fatalf("second AnnounceExpiry: %v", err)
		}
		if second {
			t.Fatal("the same expiry announced TWICE — on two replicas that is two ORDER_REJECTED " +
				"FACTs for one order, and the second contradicts nothing but confuses everything")
		}

		due, err := s.ExpiredUnannounced(context.Background(), afterTTL, 10)
		if err != nil {
			t.Fatalf("ExpiredUnannounced: %v", err)
		}
		if len(due) != 0 {
			t.Fatalf("an announced expiry is still in the queue: %v — the sweeper republishes it "+
				"every tick, forever", ids(due))
		}
	})

	// AN ANNOUNCED PROPOSAL IS NOT WORK, EVEN IF THE READER THINKS IT IS LIVE (#548).
	//
	// A pod announces expiry against its OWN clock, so one running fast can mark a
	// proposal dead that a slower reader still considers inside its window. The
	// estate has already been told that order was rejected and will not trade;
	// offering it back on the approver's queue would invite a signature on
	// something already published as dead.
	t.Run("an announced proposal never returns to the pending queue", func(t *testing.T) {
		s := newStore(t)
		born := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		p := heldOrder(t, "o-announced", "user:alice@kanz", born)
		if err := s.Put(context.Background(), p, nil); err != nil {
			t.Fatalf("Put: %v", err)
		}
		afterTTL := p.ExpiresAt.Add(time.Minute)
		if ok, err := s.AnnounceExpiry(context.Background(), "o-announced", afterTTL, nil); err != nil || !ok {
			t.Fatalf("AnnounceExpiry: ok=%v err=%v", ok, err)
		}

		// Read with a clock BEFORE the deadline: the expiry filter alone would
		// list it, so only the announced check can keep it out.
		pending, err := s.Pending(context.Background(), p.ExpiresAt.Add(-time.Minute))
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if len(pending) != 0 {
			t.Fatalf("an announced proposal is offered as work: %v — the estate was told this "+
				"order will not trade, and the queue is inviting a signature on it", ids(pending))
		}
	})

	// A LIVE PROPOSAL CANNOT BE ANNOUNCED EXPIRED. The predicate is the store's,
	// not the caller's: a sweeper with a wrong clock must not be able to kill an
	// order somebody still has time to sign.
	t.Run("a live proposal cannot be announced as expired", func(t *testing.T) {
		s := newStore(t)
		born := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		p := heldOrder(t, "o-early", "user:alice@kanz", born)
		if err := s.Put(context.Background(), p, nil); err != nil {
			t.Fatalf("Put: %v", err)
		}

		ok, err := s.AnnounceExpiry(context.Background(), "o-early", p.ExpiresAt.Add(-time.Minute), nil)
		if err != nil {
			t.Fatalf("AnnounceExpiry: %v", err)
		}
		if ok {
			t.Fatal("a proposal that had not expired was announced as expired — the approver's " +
				"remaining window is decided by the store, not by whichever pod's clock drifted")
		}
	})

	// #558. A REFUSED APPROVAL MUST BE READABLE, AND MUST NOT DECIDE ANYTHING.
	// Before this, the only trace of a refusal was a WARN in the OMS's log, which
	// the approver whose signature was refused cannot read.
	t.Run("a refusal is recorded on the proposal and the proposal stays claimable", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		p := heldOrder(t, "o-refused", "user:alice@kanz", t0)
		if err := s.Put(ctx, p, nil); err != nil {
			t.Fatalf("Put: %v", err)
		}

		at := t0.Add(time.Hour)
		recorded, err := s.RecordRefusal(ctx, "o-refused", "user:alice@kanz", RefusalSelfApproval, at)
		if err != nil {
			t.Fatalf("RecordRefusal: %v", err)
		}
		if !recorded {
			t.Fatal("a refusal against a PENDING proposal was not recorded — the approver is back " +
				"to discovering the refusal by noticing nothing happened")
		}

		got, ok, err := s.Get(ctx, "o-refused")
		if err != nil || !ok {
			t.Fatalf("Get: ok=%v err=%v", ok, err)
		}
		if got.RefusalReason != RefusalSelfApproval || got.RefusedBy != "user:alice@kanz" {
			t.Errorf("the refusal did not survive the round trip: reason=%q by=%q — the queue "+
				"cannot say why the signature was refused", got.RefusalReason, got.RefusedBy)
		}
		if !got.RefusedAt.Equal(at) {
			t.Errorf("refused_at = %v, want %v — an approver cannot tell whether the refusal "+
				"predates the terms they are reading", got.RefusedAt, at)
		}

		// THE INVARIANT #558 MUST NOT BREAK. Recording is not deciding.
		if got.Approver != "" || !got.DecidedAt.IsZero() {
			t.Fatalf("recording a refusal DECIDED the proposal (approver=%q decided_at=%v) — one "+
				"person can now destroy a colleague's pending decision by attempting their own "+
				"approval and being refused", got.Approver, got.DecidedAt)
		}
		// AND IT IS STILL ON THE QUEUE, still claimable by somebody else.
		pending, err := s.Pending(ctx, at)
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if len(pending) != 1 || pending[0].RefusalReason != RefusalSelfApproval {
			t.Fatalf("the refused proposal is not listed as work carrying its refusal: %v — a "+
				"queue that drops it is the silent drop again, one branch over", ids(pending))
		}
		claimed, err := s.Claim(ctx, "o-refused", "user:bob@kanz", at)
		if err != nil || !claimed {
			t.Fatalf("a legitimate approver could not claim a refused proposal (claimed=%v err=%v)",
				claimed, err)
		}
	})

	// A DECIDED PROPOSAL'S ROW IS THE OMS'S ONLY EVIDENCE THAT TWO PEOPLE SIGNED.
	// A late refusal must not be able to write over it, which is also what makes
	// the claim-race branch in handleApprove honest: it reports "not recorded"
	// rather than pretending it said something.
	t.Run("a refusal cannot be recorded against a decided proposal", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		p := heldOrder(t, "o-decided", "user:alice@kanz", t0)
		if err := s.Put(ctx, p, nil); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if claimed, err := s.Claim(ctx, "o-decided", "user:bob@kanz", t0.Add(time.Minute)); err != nil || !claimed {
			t.Fatalf("Claim: claimed=%v err=%v", claimed, err)
		}

		recorded, err := s.RecordRefusal(ctx, "o-decided", "user:carol@kanz", RefusalLostTheClaim, t0.Add(time.Hour))
		if err != nil {
			t.Fatalf("RecordRefusal: %v", err)
		}
		if recorded {
			t.Fatal("a refusal was written onto a DECIDED proposal — that row is the only durable " +
				"record that two people signed this order, and a late refusal just annotated it")
		}
		got, _, err := s.Get(ctx, "o-decided")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Approver != "user:bob@kanz" || got.RefusalReason != "" {
			t.Errorf("the decided row changed: approver=%q refusal=%q", got.Approver, got.RefusalReason)
		}
	})

	// A REFUSAL FOR AN ORDER NOBODY HELD MUST NOT CREATE ONE. handleApprove never
	// calls it on that branch, and the store refuses it anyway: an approval is a
	// release, never an origin, so a subject holding only the approve authority
	// must not be able to bring a proposal into existence.
	t.Run("a refusal against no proposal creates nothing", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		recorded, err := s.RecordRefusal(ctx, "o-never-held", "user:bob@kanz", RefusalSelfApproval, t0)
		if err != nil {
			t.Fatalf("RecordRefusal: %v", err)
		}
		if recorded {
			t.Fatal("recording a refusal reported success for an order that was never held")
		}
		if _, ok, err := s.Get(ctx, "o-never-held"); err != nil || ok {
			t.Fatalf("a refusal CREATED a proposal (ok=%v err=%v) — the approve authority can now "+
				"originate held orders", ok, err)
		}
	})

	// HALF A REFUSAL IS WORSE THAN NONE: it renders on the approver's queue as a
	// refusal the queue cannot describe. Both stores refuse it, and 0012's CHECK
	// refuses it again at the engine.
	t.Run("a half-written refusal is refused by both stores", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		p := heldOrder(t, "o-half", "user:alice@kanz", t0)
		if err := s.Put(ctx, p, nil); err != nil {
			t.Fatalf("Put: %v", err)
		}
		for _, c := range []struct {
			name     string
			approver string
			reason   string
			at       time.Time
		}{
			{"no subject", "  ", RefusalSelfApproval, t0},
			{"no reason", "user:bob@kanz", "", t0},
			{"no time", "user:bob@kanz", RefusalSelfApproval, time.Time{}},
		} {
			if _, err := s.RecordRefusal(ctx, "o-half", c.approver, c.reason, c.at); err == nil {
				t.Errorf("%s: a half-written refusal was accepted — the queue would show a "+
					"refusal it cannot attribute or explain", c.name)
			}
		}
	})
}

func ids(ps []OrderProposal) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

func isProposalExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrProposalExists.Error())
}

func TestMemoryProposalsHonourTheContract(t *testing.T) {
	runProposalContract(t, func(t *testing.T) ProposalStore {
		t.Helper()
		return newMemoryProposals()
	})
}

// TestMemoryProposalsPutIsAllOrNothing. The proposal and its FACT are one write:
// a hold announced to nobody is the silent drop this control exists to end, and
// a FACT for a hold that did not happen is worse.
func TestMemoryProposalsPutIsAllOrNothing(t *testing.T) {
	q := outbox.NewMemory()
	s := NewMemoryProposals(q)
	ctx := context.Background()

	rec := outbox.Record{
		Subject: EventTypePendingApproval, EventType: EventTypePendingApproval,
		Domain: Domain, PartitionKey: "o1", TenantID: "acme",
		EventTime: t0, PayloadSchemaRef: "order.v1.OrderPendingApproval:1", Payload: []byte{},
	}
	if err := s.Put(ctx, heldOrder(t, "o1", "user:alice@kanz", t0), []outbox.Record{rec}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	pending, err := q.Pending(ctx, "o1", 10)
	if err != nil {
		t.Fatalf("outbox Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("the hold enqueued %d FACTs, want 1 — the order is held and the estate has not "+
			"been told", len(pending))
	}

	// A REFUSED DUPLICATE ENQUEUES NOTHING. Otherwise a redelivery announces the
	// same hold once per delivery.
	if err := s.Put(ctx, heldOrder(t, "o1", "user:alice@kanz", t0), []outbox.Record{rec}); !isProposalExists(err) {
		t.Fatalf("duplicate Put: %v", err)
	}
	pending, err = q.Pending(ctx, "o1", 10)
	if err != nil {
		t.Fatalf("outbox Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("a REFUSED duplicate left %d FACTs queued — the hold is announced once per "+
			"redelivery", len(pending))
	}
}
