package store

import (
	"context"
	"errors"
	"testing"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/dualcontrol/proposalstore/proposalstoretest"
)

const (
	contractTenant    = "acme"
	contractPortfolio = "PF-CONTRACT"
	contractReason    = "Q3 mandate, approved by the IC"
)

// contractStore adapts this act's ProposalStore to the act-neutral shape.
//
// THE THREE ADAPTED METHODS ARE THIS ACT'S THREE REAL DIFFERENCES, and none is
// papered over. Claim takes no approver because this act DISCARDS the row — its
// durable record is the ConfigChanged FACT carrying both names. Pending and the
// expiry queue take a SCOPE, because one process holds every tenant's proposals
// and an unscoped read is a cross-tenant listing; the contract's cases all live
// in one tenant, so the adapter supplies that tenant's scope.
type contractStore struct{ ProposalStore }

func (s contractStore) Claim(ctx context.Context, id, _ string, _ time.Time) (bool, error) {
	return s.ProposalStore.Claim(ctx, id)
}

func (s contractStore) Pending(ctx context.Context, now time.Time) ([]MandateProposal, error) {
	return s.ProposalStore.Pending(ctx, TenantScope(contractTenant), now)
}

func (s contractStore) Expired(ctx context.Context, now time.Time) ([]MandateProposal, error) {
	return s.ProposalStore.Lapsed(ctx, TenantScope(contractTenant), now)
}

// contractMandate is the payload every contract proposal covers. A real mandate,
// not a zero value: Validate refuses one with no tenant or portfolio, so a
// placeholder would make every case below fail for a reason that has nothing to
// do with the contract.
func contractMandate(t *testing.T) *compliancepb.Mandate {
	t.Helper()
	return &compliancepb.Mandate{
		MandateId:   "M-CONTRACT",
		TenantId:    contractTenant,
		PortfolioId: contractPortfolio,
		Version:     7,
		EffectiveAt: timestamppb.New(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)),
	}
}

func contractHarness(newStore func(t *testing.T) ProposalStore) proposalstoretest.Harness[MandateProposal] {
	return proposalstoretest.Harness[MandateProposal]{
		New: func(t *testing.T) proposalstoretest.Store[MandateProposal] {
			t.Helper()
			return contractStore{newStore(t)}
		},
		Build: func(t *testing.T, id, proposer string, at time.Time) MandateProposal {
			t.Helper()
			m := contractMandate(t)
			digest, err := comp.MandateDigest(m, contractReason)
			if err != nil {
				t.Fatalf("MandateDigest: %v", err)
			}
			base, err := dualcontrol.Propose(id, dualcontrol.ActMandateChange,
				comp.MandateConfigKey(contractTenant, contractPortfolio), proposer, digest,
				at, dualcontrol.DefaultTTL)
			if err != nil {
				t.Fatalf("Propose: %v", err)
			}
			return MandateProposal{Proposal: base, Reason: contractReason, Mandate: m}
		},
		// FALSE, AND DELIBERATELY. Claim cannot refuse a self-approval because it
		// is never told who is approving — this act discards the row, so there is
		// nothing to record an approver on. The rule is enforced in the handler by
		// dualcontrol.Approve BEFORE the claim, which
		// TestApprove_TheProposerCannotSignTheirOwnMandateChange proves across two
		// requests, and again by Approval.Covers inside the publisher, which has no
		// argument for a lone actor at all.
		RefusesSelfApprovalAtTheStore: false,
	}
}

func TestMemoryProposalsHonourTheSharedContract(t *testing.T) {
	proposalstoretest.Run(t, contractHarness(func(*testing.T) ProposalStore {
		return NewMemoryProposals()
	}))
}

// THE CONTRACT CANNOT SEE THE SCOPE, because every case it runs lives in one
// tenant. This is the property the adapter above hides, and it is the one that
// decides whether an approver in tenant A is shown tenant B's pending mandate
// changes.
func TestPending_OneTenantsQueueNeverNamesAnothers(t *testing.T) {
	s := NewMemoryProposals()
	ctx := context.Background()
	born := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	put := func(id, tenant, portfolio string) {
		t.Helper()
		m := &compliancepb.Mandate{
			MandateId: "M1", TenantId: tenant, PortfolioId: portfolio, Version: 1,
			EffectiveAt: timestamppb.New(born),
		}
		digest, err := comp.MandateDigest(m, contractReason)
		if err != nil {
			t.Fatalf("MandateDigest: %v", err)
		}
		base, err := dualcontrol.Propose(id, dualcontrol.ActMandateChange,
			comp.MandateConfigKey(tenant, portfolio), "user:alice@kanz", digest, born,
			dualcontrol.DefaultTTL)
		if err != nil {
			t.Fatalf("Propose: %v", err)
		}
		if err := s.Put(ctx, MandateProposal{Proposal: base, Reason: contractReason, Mandate: m}); err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
	}
	put("p-acme-1", "acme", "PF-1")
	put("p-acme-2", "acme", "PF-2")
	put("p-other", "acme-archive", "PF-1")

	got, err := s.Pending(ctx, TenantScope("acme"), born.Add(time.Hour))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("acme's queue holds %d proposal(s), want 2 — got %v", len(got), proposalIDs(got))
	}
	for _, p := range got {
		if p.Mandate.GetTenantId() != "acme" {
			t.Errorf("acme's approver was shown %s's pending mandate change for portfolio %q. "+
				"The queue names the proposer, the portfolio and the reason, so this is another "+
				"fund's compliance posture read from a token that has no claim on it",
				p.Mandate.GetTenantId(), p.Mandate.GetPortfolioId())
		}
	}

	// THE PREFIX SEPARATOR IS LOAD-BEARING. "acme" and "acme-archive" are two
	// tenants, and a scope built without the trailing "/" matches both.
	if got, err := s.Pending(ctx, TenantScope("acme-archive"), born.Add(time.Hour)); err != nil {
		t.Fatalf("Pending: %v", err)
	} else if len(got) != 1 || got[0].ID != "p-other" {
		t.Errorf("acme-archive's queue = %v, want [p-other] — a scope that matches a "+
			"tenant whose name is a PREFIX of another's leaks in whichever direction the "+
			"separator was dropped", proposalIDs(got))
	}
}

// AN EMPTY SCOPE MUST MATCH NOTHING. The caller's tenant is a header value, and
// a service that reads an absent header gets "" — so the failure this closes is a
// missing principal turning into every tenant's queue at once.
func TestPending_AnEmptyScopeListsNothing(t *testing.T) {
	s := NewMemoryProposals()
	ctx := context.Background()
	born := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	m := contractMandate(t)
	digest, err := comp.MandateDigest(m, contractReason)
	if err != nil {
		t.Fatalf("MandateDigest: %v", err)
	}
	base, err := dualcontrol.Propose("p-1", dualcontrol.ActMandateChange,
		comp.MandateConfigKey(contractTenant, contractPortfolio), "user:alice@kanz", digest,
		born, dualcontrol.DefaultTTL)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if err := s.Put(ctx, MandateProposal{Proposal: base, Reason: contractReason, Mandate: m}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, scope := range []string{"", TenantScope("")} {
		got, err := s.Pending(ctx, scope, born.Add(time.Hour))
		if err != nil {
			t.Fatalf("Pending(%q): %v", scope, err)
		}
		if len(got) != 0 {
			t.Errorf("scope %q listed %v — an unscoped read is every tenant's pending "+
				"mandate changes, and it arrives by an absent principal header rather than "+
				"by anyone asking for it", scope, proposalIDs(got))
		}
	}
}

// THE STORED MANDATE MUST NOT BE MUTABLE THROUGH THE CALLER'S POINTER. The digest
// covers the serialized mandate, so a shared pointer whose rules were edited
// after the proposal makes the approver's signature cover a value the store no
// longer holds — property 2 of internal/dualcontrol, defeated by aliasing.
func TestPut_TheStoredMandateIsACopy(t *testing.T) {
	s := NewMemoryProposals()
	ctx := context.Background()
	born := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	m := contractMandate(t)
	digest, err := comp.MandateDigest(m, contractReason)
	if err != nil {
		t.Fatalf("MandateDigest: %v", err)
	}
	base, err := dualcontrol.Propose("p-alias", dualcontrol.ActMandateChange,
		comp.MandateConfigKey(contractTenant, contractPortfolio), "user:alice@kanz", digest,
		born, dualcontrol.DefaultTTL)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if err := s.Put(ctx, MandateProposal{Proposal: base, Reason: contractReason, Mandate: m}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// The proposer's own pointer, edited after proposing.
	m.Version = 99

	got, ok, err := s.Get(ctx, "p-alias")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Mandate.GetVersion() != 7 {
		t.Fatalf("the stored mandate is version %d — the proposer mutated it through their "+
			"own pointer after proposing, so the approver would sign a digest computed over "+
			"version 7 and publish version %d", got.Mandate.GetVersion(), got.Mandate.GetVersion())
	}
	// And the read-back is a copy too, or the same defect arrives one caller out.
	got.Mandate.Version = 42
	again, _, err := s.Get(ctx, "p-alias")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if again.Mandate.GetVersion() != 7 {
		t.Fatalf("a reader mutated the stored mandate to version %d — the store hands out "+
			"the value it holds rather than a copy of it", again.Mandate.GetVersion())
	}
}

// A PROPOSAL WHOSE SUBJECT DOES NOT MATCH ITS MANDATE IS REFUSED AT THE STORE.
// The subject is what every scoped read compares against, so one that disagrees
// with the mandate inside would file a change for one portfolio under another
// tenant's queue — and be approvable there.
func TestPut_ASubjectThatDoesNotMatchTheMandateIsRefused(t *testing.T) {
	s := NewMemoryProposals()
	born := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	m := contractMandate(t)
	digest, err := comp.MandateDigest(m, contractReason)
	if err != nil {
		t.Fatalf("MandateDigest: %v", err)
	}
	base, err := dualcontrol.Propose("p-mismatch", dualcontrol.ActMandateChange,
		comp.MandateConfigKey("someone-else", contractPortfolio), "user:alice@kanz", digest,
		born, dualcontrol.DefaultTTL)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	err = s.Put(context.Background(), MandateProposal{Proposal: base, Reason: contractReason, Mandate: m})
	if err == nil {
		t.Fatal("a proposal filed under one tenant's key while carrying another tenant's " +
			"mandate was stored — it lists on the wrong queue and is approvable by the " +
			"wrong people")
	}
	if !errors.Is(err, dualcontrol.ErrMalformed) {
		t.Errorf("err = %v, want dualcontrol.ErrMalformed", err)
	}
}

// A PROPOSAL WITH NO MANDATE IS REFUSED. The shared Wellformed check cannot see
// it — the record is fine — and approving it would publish nothing while
// reporting that a mandate change had been made.
func TestPut_AProposalWithNoMandateIsRefused(t *testing.T) {
	s := NewMemoryProposals()
	born := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	base, err := dualcontrol.Propose("p-nil", dualcontrol.ActMandateChange,
		comp.MandateConfigKey(contractTenant, contractPortfolio), "user:alice@kanz", "d1",
		born, dualcontrol.DefaultTTL)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if err := s.Put(context.Background(), MandateProposal{Proposal: base, Reason: contractReason}); err == nil {
		t.Fatal("a proposal carrying no mandate was stored — approving it dereferences nil " +
			"at publish time, on the path that arms every OMS replica")
	}
}

// PurgeLapsed BOUNDS THE STORE. A proposal nobody signs is never claimed, so
// without a purge every unsigned mandate change stays for the life of the pod.
func TestPurgeLapsed_TakesTheUnsignedAndLeavesTheLive(t *testing.T) {
	s := NewMemoryProposals()
	ctx := context.Background()
	born := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	build := func(id string, at time.Time) MandateProposal {
		t.Helper()
		m := contractMandate(t)
		digest, err := comp.MandateDigest(m, contractReason)
		if err != nil {
			t.Fatalf("MandateDigest: %v", err)
		}
		base, err := dualcontrol.Propose(id, dualcontrol.ActMandateChange,
			comp.MandateConfigKey(contractTenant, contractPortfolio), "user:alice@kanz", digest,
			at, dualcontrol.DefaultTTL)
		if err != nil {
			t.Fatalf("Propose: %v", err)
		}
		return MandateProposal{Proposal: base, Reason: contractReason, Mandate: m}
	}
	old := build("p-old", born)
	fresh := build("p-fresh", born.Add(72*time.Hour))
	for _, p := range []MandateProposal{old, fresh} {
		if err := s.Put(ctx, p); err != nil {
			t.Fatalf("Put %s: %v", p.ID, err)
		}
	}

	n, err := s.PurgeLapsed(ctx, born.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("PurgeLapsed: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d, want 1", n)
	}
	if _, ok, _ := s.Get(ctx, "p-fresh"); !ok {
		t.Error("the purge took a proposal that had not expired — an approver's live " +
			"decision vanished, and the proposer has no way to tell that from a refusal")
	}
	if _, ok, _ := s.Get(ctx, "p-old"); ok {
		t.Error("the purge reported taking a row and left it — the store is unbounded")
	}
}

func proposalIDs(ps []MandateProposal) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}
