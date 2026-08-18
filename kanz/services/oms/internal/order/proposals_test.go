package order

// The ProposalStore contract, against the in-process implementation.
//
// EVERY TEST HERE IS ALSO RUN AGAINST POSTGRES in proposals_postgres_test.go via
// runProposalContract, and that is deliberate: the memory store is the seam every
// service-level test in this package runs on, so a seam that accepted what
// Postgres refuses would certify behaviour production does not have. The three
// properties that CANNOT be shown here — the CHECK constraint, RLS, and two
// concurrent Claims across connections — have their own gated tests there.

import (
	"context"
	"strings"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

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

	t.Run("an unknown order is not an error", func(t *testing.T) {
		s := newStore(t)
		_, ok, err := s.Get(context.Background(), "nope")
		if err != nil || ok {
			t.Fatalf("Get of an unknown order: ok=%v err=%v", ok, err)
		}
	})

	t.Run("a second proposal for the same order is refused", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		if err := s.Put(ctx, heldOrder(t, "o-dup", "user:alice@kanz", t0), nil); err != nil {
			t.Fatalf("Put: %v", err)
		}
		err := s.Put(ctx, heldOrder(t, "o-dup", "user:bob@kanz", t0), nil)
		if err == nil {
			t.Fatal("a second proposal for one order was accepted — a redelivered SubmitOrder " +
				"would put the same decision in the approver's queue twice, and the second one " +
				"could carry a different proposer")
		}
		if !isProposalExists(err) {
			t.Fatalf("err = %v, want ErrProposalExists so the handler can ack rather than nack", err)
		}
		// The FIRST proposal must survive intact: a duplicate must not overwrite
		// the proposer an approver is being asked to differ from.
		got, _, _ := s.Get(ctx, "o-dup")
		if got.Proposer != "user:alice@kanz" {
			t.Errorf("proposer = %q, want the FIRST proposer", got.Proposer)
		}
	})

	t.Run("claim elects the caller and records the second signature", func(t *testing.T) {
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
			t.Error("decided_at is unset on a claimed proposal")
		}
		// SECOND CLAIM LOSES. The property is not "Claim returns true", it is
		// "exactly one caller is told it won".
		won2, err := s.Claim(ctx, "o-claim", "user:carol@kanz", t0.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("second Claim: %v", err)
		}
		if won2 {
			t.Fatal("a SECOND approver was also told it won — both would admit the order, so one " +
				"decision reaches a live venue twice")
		}
	})

	t.Run("a self-approval is refused and leaves the order pending", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		if err := s.Put(ctx, heldOrder(t, "o-self", "user:alice@kanz", t0), nil); err != nil {
			t.Fatalf("Put: %v", err)
		}
		// CASE AND SPACE FOLDED. One person holding both signatures by
		// capitalising a letter is the clause that fails QUIETLY: the trail then
		// shows two distinct actors, which reads as satisfied to an auditor.
		for _, spelling := range []string{"user:alice@kanz", "User:Alice@Kanz", "  user:alice@kanz  "} {
			won, err := s.Claim(ctx, "o-self", spelling, t0.Add(time.Minute))
			if won {
				t.Fatalf("%q approved an order it proposed — the audit trail would show two names "+
					"for one person", spelling)
			}
			_ = err // both a refusal and a false are acceptable; what must not happen is a win
		}
		got, ok, _ := s.Get(ctx, "o-self")
		if !ok || got.Approver != "" {
			t.Fatalf("a refused self-approval consumed or decided the proposal (ok=%v approver=%q) — "+
				"it must stay pending for somebody who may actually approve it", ok, got.Approver)
		}
	})

	t.Run("pending excludes decided and expired proposals", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		for _, id := range []string{"o-a", "o-b", "o-c"} {
			if err := s.Put(ctx, heldOrder(t, id, "user:alice@kanz", t0), nil); err != nil {
				t.Fatalf("Put %s: %v", id, err)
			}
		}
		if won, err := s.Claim(ctx, "o-b", "user:bob@kanz", t0.Add(time.Minute)); err != nil || !won {
			t.Fatalf("Claim: won=%v err=%v", won, err)
		}
		got, err := s.Pending(ctx, t0.Add(time.Hour))
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if len(got) != 2 || got[0].ID != "o-a" || got[1].ID != "o-c" {
			t.Fatalf("pending = %v, want [o-a o-c] in creation order — a DECIDED proposal on the "+
				"queue is work an approver keeps being shown after somebody did it", ids(got))
		}

		// PAST THE TTL, NOTHING IS PENDING. An expired proposal resting on the
		// queue looks like a decision that was made, and no approval can apply it.
		got, err = s.Pending(ctx, t0.Add(dualcontrol.DefaultTTL+time.Minute))
		if err != nil {
			t.Fatalf("Pending after expiry: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expired proposals are still queued: %v", ids(got))
		}
	})

	t.Run("a malformed proposal is refused rather than stored", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		base := heldOrder(t, "o-bad", "user:alice@kanz", t0)

		noProposer := base
		noProposer.Proposer = ""
		if err := s.Put(ctx, noProposer, nil); err == nil {
			t.Error("a proposal with NO PROPOSER was stored — every approver differs from \"\", so " +
				"the self-approval check would pass vacuously and one person could sign it alone")
		}

		noCommand := base
		noCommand.Command = nil
		if err := s.Put(ctx, noCommand, nil); err == nil {
			t.Error("a proposal with no command was stored — approving it could admit nothing")
		}

		bornExpired := base
		bornExpired.ExpiresAt = bornExpired.CreatedAt
		if err := s.Put(ctx, bornExpired, nil); err == nil {
			t.Error("a proposal born expired was stored — it can never be approved and would sit " +
				"in the queue looking like work nobody did")
		}
	})

	t.Run("an empty approver cannot claim", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		if err := s.Put(ctx, heldOrder(t, "o-anon", "user:alice@kanz", t0), nil); err != nil {
			t.Fatalf("Put: %v", err)
		}
		won, err := s.Claim(ctx, "o-anon", "", t0.Add(time.Minute))
		if won {
			t.Fatal("an UNNAMED approver claimed the order — the audit row would say two people " +
				"signed while naming one")
		}
		if err == nil {
			t.Error("an empty approver was refused silently; an approval must come from an " +
				"authenticated subject and saying so is the whole point")
		}
	})
	// EXPIRY IS A SEPARATE QUEUE FROM THE PENDING ONE, AND THAT IS THE POINT (#539).
	//
	// Pending() filters an expired proposal OUT — it is not work anybody can do.
	// Nothing then looked at those rows again, so a proposal nobody signed became
	// invisible rather than terminal: the trader who submitted it saw
	// ORDER_PENDING_APPROVAL and then nothing, ever. These are the properties that
	// let a sweeper find them and announce each one exactly once.
	t.Run("an expired proposal leaves the pending queue and enters the expiry queue", func(t *testing.T) {
		s := newStore(t)
		born := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		p := heldOrder(t, "o-exp", "user:alice@kanz", born)
		if err := s.Put(context.Background(), p, nil); err != nil {
			t.Fatalf("Put: %v", err)
		}

		afterTTL := p.ExpiresAt.Add(time.Minute)

		pending, err := s.Pending(context.Background(), afterTTL)
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if len(pending) != 0 {
			t.Fatalf("an expired proposal is still offered as work: %v", ids(pending))
		}

		due, err := s.ExpiredUnannounced(context.Background(), afterTTL, 10)
		if err != nil {
			t.Fatalf("ExpiredUnannounced: %v", err)
		}
		if got := ids(due); len(got) != 1 || got[0] != "o-exp" {
			t.Fatalf("expiry queue = %v, want [o-exp] — a proposal that expired unsigned is in no "+
				"queue at all, which is how it dies in silence", got)
		}
	})

	t.Run("a proposal that has not expired is not due for announcement", func(t *testing.T) {
		s := newStore(t)
		born := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		p := heldOrder(t, "o-live", "user:alice@kanz", born)
		if err := s.Put(context.Background(), p, nil); err != nil {
			t.Fatalf("Put: %v", err)
		}

		due, err := s.ExpiredUnannounced(context.Background(), p.ExpiresAt.Add(-time.Minute), 10)
		if err != nil {
			t.Fatalf("ExpiredUnannounced: %v", err)
		}
		if len(due) != 0 {
			t.Fatalf("a live proposal was announced as expired: %v — the approver still had time "+
				"and the order was killed under them", ids(due))
		}
	})

	// AN APPROVED PROPOSAL NEVER EXPIRES. It was decided; the deadline stopped
	// mattering the moment somebody signed it. Announcing one would tell the estate
	// an order that traded was abandoned.
	t.Run("a decided proposal is never due for expiry", func(t *testing.T) {
		s := newStore(t)
		born := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		p := heldOrder(t, "o-signed", "user:alice@kanz", born)
		if err := s.Put(context.Background(), p, nil); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if ok, err := s.Claim(context.Background(), "o-signed", "user:bob@kanz", born.Add(time.Hour)); err != nil || !ok {
			t.Fatalf("Claim: ok=%v err=%v", ok, err)
		}

		due, err := s.ExpiredUnannounced(context.Background(), p.ExpiresAt.Add(time.Hour), 10)
		if err != nil {
			t.Fatalf("ExpiredUnannounced: %v", err)
		}
		if len(due) != 0 {
			t.Fatalf("a SIGNED proposal was queued for expiry: %v", ids(due))
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
