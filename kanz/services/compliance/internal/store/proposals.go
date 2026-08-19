// Package store holds the compliance service's server-side state. Today that is
// one thing: the mandate changes waiting for a second signature (#562).
//
// # Why a mandate proposal has to live on a server at all
//
// cmd/kanz-mandate already took two invocations, and it carried the proposal
// between them in a FILE. That made a unilateral change DETECTABLE — the FACT
// records four eyes and the digest covers the exact mandate — and it could never
// make one PREVENTABLE: both invocations run on one operator's machine under one
// SVID, and a file is a carrier rather than a signature.
//
// Two separately authenticated REQUESTS cannot pass a file. The proposal has to
// rest somewhere the second request can find it, which is here.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	"google.golang.org/protobuf/proto"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/dualcontrol/proposalstore"
)

// MandateProposal is a mandate change waiting for its second signature: the
// dual-control record plus the exact mandate and reason the approval covers.
//
// THE PAYLOAD TRAVELS WITH THE PROPOSAL, which is the whole difference between
// this and the CLI. kanz-mandate approve is handed the mandate file again and
// re-derives the digest from it; the digest catches a swap, but "the approver's
// client supplies the value" is then the NORMAL path, and a control whose safety
// rests on a check that fires on the happy path is one refactor from ceremony.
// Here the approver names a proposal id and signs what the server already holds.
type MandateProposal struct {
	dualcontrol.Proposal
	// Reason is the sentence that goes in the audit trail. It is INSIDE the
	// digest (comp.MandateDigest hashes it), so the recorded justification cannot
	// change after the second signature either.
	Reason string
	// Mandate is what will be published if this is approved.
	Mandate *compliancepb.Mandate
}

// Record, WithRecord, Decided, Copy and Validate make a mandate proposal storable
// by internal/dualcontrol/proposalstore — the mechanics this act shares with the
// pricing override and the held order, spelled once (#562).
func (p MandateProposal) Record() dualcontrol.Proposal { return p.Proposal }

// WithRecord returns a copy carrying r. It is how the shared contract builds its
// malformed cases without knowing what a mandate is.
func (p MandateProposal) WithRecord(r dualcontrol.Proposal) MandateProposal {
	p.Proposal = r
	return p
}

// Decided is ALWAYS FALSE HERE, and it is this act's answer rather than a stub.
// Claim removes the row, and the durable record of an applied mandate change is
// the ConfigChanged FACT on the compacted MANDATE stream carrying changed_by and
// approved_by. A second copy of that fact here would be a second answer that can
// disagree with the stream every OMS and compliance replica arms from.
func (p MandateProposal) Decided() bool { return false }

// Copy is a DEEP copy, and for a protobuf that means proto.Clone.
//
// A STORED *Mandate THE CALLER CAN STILL MUTATE IS NOT A RECORD OF WHAT WAS
// PROPOSED. The digest covers the serialized mandate, so a shared pointer whose
// rules were edited after the proposal would make the approver's signature cover
// a value the store no longer holds — the exact binding property 2 of
// internal/dualcontrol exists to guarantee.
func (p MandateProposal) Copy() MandateProposal {
	if p.Mandate != nil {
		p.Mandate = proto.Clone(p.Mandate).(*compliancepb.Mandate)
	}
	return p
}

// Validate refuses a proposal this store cannot honestly hold.
//
// IT RUNS IN THE BACKEND, not in the handler, which is the lesson #562 was filed
// on: datamaster's two backends accepted different things precisely because the
// checks lived at the call sites. Wellformed covers the shared record; the two
// clauses below are this act's own, and both are refusals that would otherwise
// surface as a nil dereference at publish time.
func (p MandateProposal) Validate() error {
	if err := proposalstore.Wellformed(p.Proposal); err != nil {
		return err
	}
	switch {
	case p.Mandate == nil:
		return fmt.Errorf("%w: proposal %s carries no mandate, so approving it would publish nothing",
			dualcontrol.ErrMalformed, p.ID)
	case p.Mandate.GetTenantId() == "" || p.Mandate.GetPortfolioId() == "":
		// A MANDATE WITH NO PORTFOLIO GOVERNS NOTHING AND CANNOT BE ROUTED. The
		// subject it would publish on is derived from both, so an empty one is a
		// FACT nobody consumes — a mandate change that reports success and arms
		// no replica, which is the silent failure this whole control exists to end.
		return fmt.Errorf("%w: proposal %s names no tenant or no portfolio, so its mandate would "+
			"govern nothing", dualcontrol.ErrMalformed, p.ID)
	case p.Subject != comp.MandateConfigKey(p.Mandate.GetTenantId(), p.Mandate.GetPortfolioId()):
		// THE SUBJECT IS THE TENANT GATE. Every read below is scoped by comparing
		// it against a key built from the CALLER'S authenticated tenant, so a
		// subject that does not match the mandate inside would let a proposal for
		// one portfolio be listed and approved under another's name.
		return fmt.Errorf("%w: proposal %s is filed under %q but carries the mandate for %q",
			dualcontrol.ErrMalformed, p.ID, p.Subject,
			comp.MandateConfigKey(p.Mandate.GetTenantId(), p.Mandate.GetPortfolioId()))
	}
	return nil
}

// ErrNoProposal is returned when a proposal id is unknown — it never existed, it
// was already decided, or another tenant owns it. The three are deliberately
// indistinguishable: distinguishing them makes the surface an oracle for other
// tenants' pending mandate changes.
var ErrNoProposal = errors.New("store: no such pending mandate proposal")

// ProposalStore persists pending mandate-change proposals.
//
// THERE IS NO "APPLIED" FLAG. The durable record of what happened is the
// ConfigChanged FACT carrying both names, on a compacted stream that is already
// the platform's answer to "which mandate governs this portfolio". A proposal is
// therefore CLAIMED — removed — when it is published or rejected.
type ProposalStore interface {
	// Put holds a proposal, returning proposalstore.ErrExists if the id is held.
	Put(ctx context.Context, p MandateProposal) error
	// Get reads without consuming, so a refused self-approval leaves the proposal
	// pending for somebody who may legitimately sign it.
	Get(ctx context.Context, proposalID string) (MandateProposal, bool, error)
	// Claim atomically removes a proposal and reports whether THIS caller got it.
	//
	// IT IS THE SERIALISATION POINT, and the reason it is not a Delete: two
	// approvers acting on one pending mandate change at the same moment both pass
	// every check — different people, matching digest, neither expired — and both
	// would publish, putting two ConfigChanged FACTs for one decision onto a
	// compacted stream where the last one wins. Whoever claims it publishes; the
	// other is told it is already decided.
	Claim(ctx context.Context, proposalID string) (bool, error)
	// Pending lists proposals under scope that have not expired at now, oldest
	// first, so a change nobody signed is VISIBLE rather than silently dropped.
	//
	// THE SCOPE IS A REQUIRED ARGUMENT, NOT AN OPTIONAL FILTER. This store holds
	// every tenant's proposals in one map, so an unscoped list would name the
	// proposer, the portfolio and the reason of another tenant's pending mandate
	// change. TenantScope builds the one value a caller may pass.
	Pending(ctx context.Context, scope string, now time.Time) ([]MandateProposal, error)
	// Lapsed lists proposals under scope that expired without a signature and have
	// not been purged, so "nobody signed it" is a state the proposer can SEE
	// rather than infer from an absence (#563's finding, applied to act two before
	// it could be repeated).
	Lapsed(ctx context.Context, scope string, now time.Time) ([]MandateProposal, error)
	// PurgeLapsed removes proposals that expired before cutoff and reports how
	// many went. It is what bounds the store: a proposal nobody signs is never
	// claimed, so without this every unsigned mandate change stays forever.
	PurgeLapsed(ctx context.Context, cutoff time.Time) (int64, error)
}

// MemoryProposals is the in-process ProposalStore.
//
// # It is the ONLY backend, and that is a posture rather than an omission
//
// The compliance deployment is replicas: 1 with strategy Recreate, and that pin
// is a CORRECTNESS bound rather than a resource choice — the monitor holds each
// portfolio's whole book in memory and two pods would each sum a partial one
// (infra/deploy/compliance-deploy.yaml). So the multi-replica failure that makes
// an in-process proposal store wrong elsewhere — the second signature landing on
// a pod that never saw the first — cannot arise here without breaking a bound
// this service already cannot cross.
//
// WHAT IT DOES COST, stated rather than discovered: a restart drops every pending
// proposal. That is a re-propose, not a lost decision — nothing was published, the
// mandate in force is unchanged, and the compacted MANDATE stream is untouched.
// The composition root logs the posture at boot so it is a stated fact rather than
// something an operator infers from a proposal that vanished.
//
// THE MECHANICS ARE THE SHARED ONES (#562). One map, one lock, one copy-on-read
// discipline and one total order live in internal/dualcontrol/proposalstore, so
// this act cannot drift from the other two. What stays here is this act's
// DISPOSITION — Claim removes the row — and its tenant scoping.
type MemoryProposals struct {
	core *proposalstore.Memory[MandateProposal]
}

func NewMemoryProposals() *MemoryProposals {
	return &MemoryProposals{core: proposalstore.NewMemory[MandateProposal]()}
}

// Put holds a proposal. There is nothing to commit alongside it: this act
// announces no FACT for a HELD mandate change — the only thing it publishes is
// the mandate itself, once two people have signed — so the announcement argument
// the OMS's equivalent carries has no counterpart here.
func (m *MemoryProposals) Put(ctx context.Context, p MandateProposal) error {
	return m.core.Insert(ctx, p, nil)
}

func (m *MemoryProposals) Get(ctx context.Context, id string) (MandateProposal, bool, error) {
	return m.core.Get(ctx, id)
}

// Claim REMOVES the proposal. See ProposalStore.Claim: the removal is the
// serialisation point, and this act keeps no approver on the row because the FACT
// carries both names.
func (m *MemoryProposals) Claim(ctx context.Context, id string) (bool, error) {
	return m.core.Remove(ctx, id)
}

func (m *MemoryProposals) Pending(ctx context.Context, scope string, now time.Time) ([]MandateProposal, error) {
	return m.core.Select(ctx, func(p MandateProposal) bool {
		return scoped(p, scope) && p.Pending(now)
	})
}

// Lapsed is NOT !Pending. Pending is false for a claimed proposal too, and a
// claimed one is not in this store to begin with — expiry is the only way a row
// survives without being pending. proposalstore.Expired is the same comparison
// every other act's expiry queue uses, so the three cannot drift on what "past
// its deadline" means.
func (m *MemoryProposals) Lapsed(ctx context.Context, scope string, now time.Time) ([]MandateProposal, error) {
	return m.core.Select(ctx, func(p MandateProposal) bool {
		return scoped(p, scope) && proposalstore.Expired(p.Proposal, now)
	})
}

// PurgeLapsed is NOT tenant-scoped, deliberately: it is the estate's own
// housekeeping rather than a caller's read, and scoping it would leave every
// tenant that stopped proposing with rows nothing ever removes.
func (m *MemoryProposals) PurgeLapsed(ctx context.Context, cutoff time.Time) (int64, error) {
	return m.core.Purge(ctx, func(p MandateProposal) bool { return p.ExpiresAt.Before(cutoff) })
}

// TenantScope is the ONE value a queue read may be scoped by: every mandate
// config key that belongs to this tenant.
//
// IT IS A FUNCTION SO NOBODY BUILDS THE PREFIX BY HAND. comp.MandateConfigKey
// joins tenant and portfolio with a "/", so the tenant's prefix is that key with
// an empty portfolio — and the trailing separator is what stops tenant "acme"
// matching tenant "acme-archive". Spelling it at each call site is how one of
// them ends up without the separator, which is a cross-tenant listing that reads
// as a working queue.
func TenantScope(tenantID string) string { return comp.MandateConfigKey(tenantID, "") }

// scoped reports whether p falls under the caller's scope.
//
// AN EMPTY SCOPE MATCHES NOTHING, which is the direction that fails closed. The
// obvious spelling — treat "" as "everything" — is how an unauthenticated read
// becomes a cross-tenant listing: the caller's tenant is a string, and a service
// that reads it from a header gets "" when the header is absent. TenantScope("")
// is not empty, so this guards the arguments a caller composes by hand as well.
func scoped(p MandateProposal, scope string) bool {
	return scope != "" && scope != comp.MandateConfigKeyPrefix+"/" &&
		strings.HasPrefix(p.Subject, scope)
}

var _ ProposalStore = (*MemoryProposals)(nil)
