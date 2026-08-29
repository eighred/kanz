package order

import (
	"context"
	"errors"
	"testing"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/pkg/bus"
)

// A CLAIM CANNOT OUTRUN ITS OWN EXPIRY (#796).
//
// # The asymmetry these tests close
//
// AnnounceExpiry has always defended against a racing claim — its UPDATE carries
// `approver = ''`. Claim defended against nothing in the other direction: its
// predicate was `approver = '' AND proposer <> approver`, and MemoryProposals
// had the same omission, so the two stores AGREED. That is what made it a gap in
// the state machine rather than a Postgres typo.
//
// The window needs no clock skew and no second process. handleApprove captures
// `now` BEFORE the Proposals().Get round trip, dualcontrol.Approve checks the
// deadline against that captured value, and the Claim runs later still, while
// ExpireProposals runs on its own ticker in the composition root.
//
//	T-10ms  the approval arrives; Approve passes, the proposal is live
//	T+5ms   the ticker commits ORDER_REJECTED / DUAL_CONTROL_EXPIRED and a
//	        REJECTED CommandOutcome, and sets expiry_announced_at — NOT approver
//	T+20ms  Claim runs, sees approver = '' and SUCCEEDS
//
// The estate then received a terminal REJECTED and afterwards ORDER_APPROVED,
// ORDER_ACCEPTED and fills for the same order_id — two terminal answers to one
// command, with capital moving on an approval the platform had declared dead.
//
// # Both stores, one property
//
// Every case below runs against BOTH backends. A fix spelled only in SQL would
// leave every service-level test in this package — which runs on the memory seam
// — certifying the defect.

// expiryClaimStores is the pair every case here runs against. The Postgres arm
// skips without TEST_POSTGRES_URL, and skipping is not passing.
func expiryClaimStores(t *testing.T) map[string]func(*testing.T) ProposalStore {
	return map[string]func(*testing.T) ProposalStore{
		"memory": func(*testing.T) ProposalStore { return newMemoryProposals() },
		"postgres": func(t *testing.T) ProposalStore {
			t.Helper()
			return NewPostgres(newPool(t)).Proposals()
		},
	}
}

// heldProposal puts one proposal that expired a moment ago — the population the
// sweeper is about to announce and a late approval is about to reach.
func heldProposal(t *testing.T, ctx context.Context, store ProposalStore, orderID string, expiresAt time.Time) {
	t.Helper()
	err := store.Put(ctx, OrderProposal{
		Proposal: dualcontrol.Proposal{
			ID:        orderID,
			Subject:   orderID,
			Act:       dualcontrol.ActOrderSubmission,
			Proposer:  "user:maker",
			Digest:    "digest-" + orderID,
			CreatedAt: expiresAt.Add(-time.Hour),
			ExpiresAt: expiresAt,
		},
		PortfolioID: "pf1",
		Command: &orderpb.SubmitOrder{
			OrderId:     orderID,
			PortfolioId: "pf1",
			Metadata:    &commandpb.CommandMetadata{TargetId: orderID, Issuer: "user:maker"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("put proposal: %v", err)
	}
}

// claimFact is the ORDER_APPROVED record Claim requires. It is built through the
// emitter's own builder so it cannot drift in shape from what production writes.
func claimFact(t *testing.T, ctx context.Context, orderID string, at time.Time) []outbox.Record {
	t.Helper()
	rec, err := outbox.From(ctx, NewEmitter(nil).event(EventTypeApproved, orderID, at,
		&orderpb.OrderApproved{OrderId: orderID}))
	if err != nil {
		t.Fatalf("build approval record: %v", err)
	}
	return []outbox.Record{rec}
}

// expiryRecords reuses the package's own well-formed expiry announcement, so the
// record this test commits cannot drift in shape from the one the sweeper writes.
func expiryRecords(orderID string) []outbox.Record { return []outbox.Record{expiryFact(orderID)} }

// THE ISSUE'S OWN "VERIFIED WHEN": AnnounceExpiry, then Claim for the same
// order_id, must see Claim return FALSE. It returned true.
func TestAClaimIsRefusedOnceTheExpiryHasBeenAnnounced(t *testing.T) {
	for name, open := range expiryClaimStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := bus.WithTenantID(context.Background(), "acme")
			store := open(t)
			orderID := "o-796-" + name
			deadline := time.Now().UTC().Add(-time.Minute)
			heldProposal(t, ctx, store, orderID, deadline)

			at := time.Now().UTC()
			announced, err := store.AnnounceExpiry(ctx, orderID, at, expiryRecords(orderID))
			if err != nil {
				t.Fatalf("announce expiry: %v", err)
			}
			if !announced {
				t.Fatal("the expiry was not announced, so the race this test sets up never happened " +
					"and the assertion below would prove nothing")
			}

			// The late approval. Everything about it is legitimate — a different
			// person, the right digest — and the ONLY thing wrong with it is that
			// the estate has already been told this order is dead.
			claimed, err := store.Claim(ctx, orderID, "user:checker", at, claimFact(t, ctx, orderID, at))
			if err != nil && !errors.Is(err, dualcontrol.ErrSelfApproval) {
				t.Fatalf("claim: %v", err)
			}
			if claimed {
				t.Fatal("the claim SUCCEEDED on a proposal whose expiry was already announced.\n\n" +
					"The estate received a terminal ORDER_REJECTED / DUAL_CONTROL_EXPIRED and will " +
					"now receive ORDER_APPROVED, ORDER_ACCEPTED and fills for the same order_id — " +
					"two terminal answers to one command, and capital moving on a maker-checker " +
					"approval the platform had already declared dead. Every downstream projection " +
					"that folds terminality is reconstructing a history the platform did not have.")
			}
		})
	}
}

// THE MIRROR, WHICH ALREADY HELD, and is here so the pair is asserted in both
// directions rather than only the one that was broken. Whichever commits first
// excludes the other, and rows-affected stays the verdict on either side.
func TestAnExpiryIsRefusedOnceTheClaimHasLanded(t *testing.T) {
	for name, open := range expiryClaimStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := bus.WithTenantID(context.Background(), "acme")
			store := open(t)
			orderID := "o-796-mirror-" + name
			deadline := time.Now().UTC().Add(-time.Minute)
			heldProposal(t, ctx, store, orderID, deadline)

			at := time.Now().UTC()
			claimed, err := store.Claim(ctx, orderID, "user:checker", at, claimFact(t, ctx, orderID, at))
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if !claimed {
				t.Fatal("the claim was refused, so the mirror this test asserts was never set up")
			}

			announced, err := store.AnnounceExpiry(ctx, orderID, at, expiryRecords(orderID))
			if err != nil {
				t.Fatalf("announce expiry: %v", err)
			}
			if announced {
				t.Fatal("an approved order's expiry was announced. The deadline stopped mattering " +
					"the moment somebody signed, and the estate would be told a live order was rejected")
			}
		})
	}
}

// NON-VACUITY, AND IT IS THE ARM THAT MATTERS MOST HERE. A proposal nobody has
// announced an expiry for is still claimable — the refusal above is about the
// ANNOUNCEMENT, not about approvals being switched off. Without this, both cases
// above would pass on a Claim that refused everything.
func TestAClaimStillSucceedsWhileNoExpiryHasBeenAnnounced(t *testing.T) {
	for name, open := range expiryClaimStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := bus.WithTenantID(context.Background(), "acme")
			store := open(t)
			orderID := "o-796-live-" + name
			heldProposal(t, ctx, store, orderID, time.Now().UTC().Add(time.Hour))

			at := time.Now().UTC()
			claimed, err := store.Claim(ctx, orderID, "user:checker", at, claimFact(t, ctx, orderID, at))
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if !claimed {
				t.Fatal("a live, unannounced proposal could not be claimed — the expiry predicate is " +
					"refusing approvals it must not, and every held order would now be unreleasable")
			}
		})
	}
}

// expiry_announced_at MUST BE READABLE FROM BOTH STORES (#796).
//
// The memory store SET the field and the durable one never SELECTed it, so
// OrderProposal.ExpiryAnnouncedAt was populated by one backend and permanently
// zero on the other. handleApprove now branches on it to tell an approver whether
// they lost to a signature or to the deadline — two refusals that demand opposite
// responses — and with the old read that branch would have taken the memory
// store's answer in every test and the wrong one in production.
func TestBothStoresReportWhetherTheExpiryWasAnnounced(t *testing.T) {
	for name, open := range expiryClaimStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := bus.WithTenantID(context.Background(), "acme")
			store := open(t)
			orderID := "o-796-read-" + name
			heldProposal(t, ctx, store, orderID, time.Now().UTC().Add(-time.Minute))

			before, ok, err := store.Get(ctx, orderID)
			if err != nil || !ok {
				t.Fatalf("get: %v (found=%v)", err, ok)
			}
			if !before.ExpiryAnnouncedAt.IsZero() {
				t.Fatalf("a freshly held proposal reports its expiry announced at %v", before.ExpiryAnnouncedAt)
			}

			at := time.Now().UTC().Truncate(time.Millisecond)
			if _, err := store.AnnounceExpiry(ctx, orderID, at, expiryRecords(orderID)); err != nil {
				t.Fatalf("announce expiry: %v", err)
			}

			after, ok, err := store.Get(ctx, orderID)
			if err != nil || !ok {
				t.Fatalf("get after announce: %v (found=%v)", err, ok)
			}
			if after.ExpiryAnnouncedAt.IsZero() {
				t.Fatal("the expiry was announced and Get still reports it was not. Anything that " +
					"branches on this field reads one answer from the memory seam every test runs " +
					"on and the opposite one in production.")
			}
			if after.Approver != "" {
				t.Fatalf("announcing an expiry set approver = %q — it must not decide the proposal", after.Approver)
			}
		})
	}
}
