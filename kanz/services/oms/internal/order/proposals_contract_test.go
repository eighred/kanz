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

	"github.com/eighred/kanz/internal/dualcontrol/proposalstore/proposalstoretest"
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
