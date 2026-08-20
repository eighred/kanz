package order

// THE SHARED PROPOSAL-STORE CONTRACT, against both of this act's backends (#562).
//
// This act already ran a store contract against both of its backends, which is
// why its two backends agree. The contract was the thing worth copying to act
// one, and it was the thing that did not get copied — so the act-neutral half of
// it now lives in internal/dualcontrol/proposalstore and every act runs it.
//
// What stays in runProposalContract is what only an ORDER proposal can be wrong
// about: the command and its digest, the recorded second signature, the expiry
// announcement, and #558's refusal record.

import (
	"context"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dualcontrol/proposalstore/proposalstoretest"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/pkg/bus"
)

// contractStore adapts this act's ProposalStore to the act-neutral shape.
//
// THE TWO ADAPTED METHODS ARE THE TWO REAL DIFFERENCES. Put carries the FACT
// announcing the held order, because here the hold and its announcement are one
// transaction (#292) — the contract has no announcement to make and passes nil.
// The expiry queue is ExpiredUnannounced because this act announces a terminal
// FACT exactly once (#547) rather than listing lapsed proposals.
type contractStore struct{ ProposalStore }

func (s contractStore) Put(ctx context.Context, p OrderProposal) error {
	return s.ProposalStore.Put(ctx, p, nil)
}

// Claim supplies the ORDER_APPROVED record this act's Claim requires (#410,
// clause (d)). The record is built here rather than from a dualcontrol.Approval
// ON PURPOSE: the contract's job is claim ARBITRATION — which of two callers
// wins, and what the store itself refuses — and it has to be able to hand Claim
// a well-formed announcement for approvers the dual-control rule would reject
// first, including the empty one whose refusal AT THE STORE is the property
// under test. Routing it through Approve would make dualcontrol refuse before
// the store ever saw the call, and the assertion would then prove nothing about
// the store.
//
// WHAT the winning claim announces is this act's own property, and it is
// asserted off a real broker in approval_fact_integration_test.go.
func (s contractStore) Claim(ctx context.Context, id, approver string, at time.Time) (bool, error) {
	fact, err := contractApprovalFact(ctx, id, at)
	if err != nil {
		return false, err
	}
	return s.ProposalStore.Claim(ctx, id, approver, at, []outbox.Record{fact})
}

// contractApprovalFact builds a well-formed stand-in through the emitter's own
// event builder, so the record the contract enqueues cannot drift in shape from
// the one production writes. The tenant comes off ctx if a delivery put one
// there and is otherwise supplied here — outbox.From refuses a record without
// one, and the contract's ctx is a bare Background.
func contractApprovalFact(ctx context.Context, orderID string, at time.Time) (outbox.Record, error) {
	if bus.TenantIDFromContext(ctx) == "" {
		ctx = bus.WithTenantID(ctx, "contract-tenant")
	}
	return outbox.From(ctx, NewEmitter(nil).event(EventTypeApproved, orderID, at,
		&orderpb.OrderApproved{OrderId: orderID}))
}

func (s contractStore) Expired(ctx context.Context, now time.Time) ([]OrderProposal, error) {
	return s.ProposalStore.ExpiredUnannounced(ctx, now, 0)
}

func contractHarness(newStore func(t *testing.T) ProposalStore) proposalstoretest.Harness[OrderProposal] {
	return proposalstoretest.Harness[OrderProposal]{
		New: func(t *testing.T) proposalstoretest.Store[OrderProposal] {
			t.Helper()
			return contractStore{newStore(t)}
		},
		Build: func(t *testing.T, id, proposer string, at time.Time) OrderProposal {
			t.Helper()
			return heldOrder(t, id, proposer, at)
		},
		// TRUE. This act KEEPS the row after an approval, so Claim writes the
		// record an auditor reads and has to be the last line before it — the
		// database refuses the same thing again at 0009's CHECK.
		RefusesSelfApprovalAtTheStore: true,
	}
}

func TestMemoryProposalsHonourTheSharedContract(t *testing.T) {
	proposalstoretest.Run(t, contractHarness(func(*testing.T) ProposalStore {
		return newMemoryProposals()
	}))
}

// TestPostgresProposalsHonourTheSharedContract runs the identical assertions
// against the engine. Gated on TEST_POSTGRES_URL; skipping is not passing.
func TestPostgresProposalsHonourTheSharedContract(t *testing.T) {
	proposalstoretest.Run(t, contractHarness(func(t *testing.T) ProposalStore {
		t.Helper()
		return NewPostgres(newPool(t)).Proposals()
	}))
}
