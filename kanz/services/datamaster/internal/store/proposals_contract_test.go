package store

// THE SHARED PROPOSAL-STORE CONTRACT, against both of this act's backends (#562).
//
// WHY THIS FILE EXISTS AT ALL. Before it, MemoryProposals had no test of any
// kind, and it accepted three things PostgresProposals refuses: a second
// proposal for an id already held (the map overwrote the first, so a redelivery
// could replace the proposer an approver is being asked to differ from), a
// proposal with no proposer (every approver differs from "", so the
// self-approval check passes vacuously), and a nil chosen price (which panicked
// rather than being refused). All three were invisible because the seam nothing
// tested was the seam every server-level test in this package runs on.
//
// The contract is act-neutral and lives in internal/dualcontrol/proposalstore,
// so the OMS's two backends and act two's are held to the same assertions. What
// is specific to the pricing override — the CHECK constraints, RLS, the
// retention purge, ApplyOverride — stays in proposals_postgres_test.go.

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/dualcontrol/proposalstore/proposalstoretest"
)

// contractStore adapts this act's ProposalStore to the act-neutral shape.
//
// THE TWO ADAPTED METHODS ARE THE TWO REAL DIFFERENCES, and neither is papered
// over: Claim takes no approver because this act DISCARDS the row (its durable
// record is the append-only exception_overrides row carrying both names), and
// the expiry queue is Lapsed because this act announces nothing and lists the
// lapsed proposal until a retention purge takes it (#563).
type contractStore struct{ ProposalStore }

func (s contractStore) Claim(ctx context.Context, id, _ string, _ time.Time) (bool, error) {
	return s.ProposalStore.Claim(ctx, id)
}

func (s contractStore) Expired(ctx context.Context, now time.Time) ([]OverrideProposal, error) {
	return s.ProposalStore.Lapsed(ctx, now)
}

// contractException is the exception every contract proposal overrides. The
// Postgres table has a foreign key to it, so a proposal for an exception that
// does not exist is refused — which is correct, and would otherwise make every
// case below fail for a reason that has nothing to do with the contract.
const contractException = "CONTRACT:PRICE_TOLERANCE:ICE"

func contractHarness(newStore func(t *testing.T) ProposalStore) proposalstoretest.Harness[OverrideProposal] {
	return proposalstoretest.Harness[OverrideProposal]{
		New: func(t *testing.T) proposalstoretest.Store[OverrideProposal] {
			t.Helper()
			return contractStore{newStore(t)}
		},
		Build: func(t *testing.T, id, proposer string, at time.Time) OverrideProposal {
			t.Helper()
			rat, ok := new(big.Rat).SetString("130")
			if !ok {
				t.Fatal("bad price literal")
			}
			base, err := dualcontrol.Propose(id, dualcontrol.ActPricingOverride, contractException,
				proposer, PayloadDigest(contractException, "vendor confirmed", rat), at,
				dualcontrol.DefaultTTL)
			if err != nil {
				t.Fatalf("Propose: %v", err)
			}
			return OverrideProposal{Proposal: base, Reason: "vendor confirmed", ChosenPrice: rat}
		},
		// FALSE, AND DELIBERATELY. Claim cannot refuse a self-approval because it
		// is never told who is approving — see contractStore.Claim. The rule is
		// enforced by handleApproveOverride through dualcontrol.Approve before the
		// claim, and by 0004's actor <> approver CHECK on the row an auditor reads,
		// which TestPostgresOverride_TheDatabaseRefusesASelfApproval proves.
		RefusesSelfApprovalAtTheStore: false,
	}
}

func TestMemoryProposalsHonourTheSharedContract(t *testing.T) {
	proposalstoretest.Run(t, contractHarness(func(*testing.T) ProposalStore {
		return NewMemoryProposals()
	}))
}

// TestPostgresProposalsHonourTheSharedContract runs the identical assertions
// against the engine. Gated on TEST_POSTGRES_URL; skipping is not passing.
func TestPostgresProposalsHonourTheSharedContract(t *testing.T) {
	proposalstoretest.Run(t, contractHarness(func(t *testing.T) ProposalStore {
		t.Helper()
		pool := newPool(t)
		seedException(t, pool, contractException)
		return NewPostgresProposals(pool)
	}))
}
