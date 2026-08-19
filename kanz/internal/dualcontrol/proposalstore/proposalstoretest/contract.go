// Package proposalstoretest is the contract every dual-control proposal store
// must satisfy, whichever act owns it and whichever backend implements it.
//
// # Why it is a package and not a helper in each act
//
// The OMS already ran its store contract against both of its backends, and that
// is why its two backends agree. datamaster ran nothing against its in-process
// store at all, and its two backends disagreed in three ways (see
// proposalstore's doc). The contract was the thing worth copying and it was the
// thing that did not get copied.
//
// So it lives here, takes a Harness, and runs against every backend of every
// shipped act — four when this landed, FIVE since #562 wired act two's
// mandate-change store, which added its backend by writing a thirty-line Harness
// rather than by writing a third set of mechanics. That is the point: a new act
// cannot quietly ship a store that accepts what the others refuse.
//
// ACT TWO ADDED ONE BACKEND AND NOT TWO, and the asymmetry is worth naming so
// nobody reads it as a missing Postgres store. The compliance service has no
// database and is pinned to replicas: 1 for the post-trade monitor's own
// correctness, so its proposals are in-process by a stated posture — see
// services/compliance/internal/store. A durable backend there is a change to that
// pin first and a Harness second.
//
// # What it deliberately does NOT assert
//
// Only the properties EVERY act must have. An act's own disposition — the OMS's
// exactly-once expiry announcement, datamaster's retention purge — is proven by
// that act's own tests, because asserting either here would make the other act
// wrong. Capability fields on the Harness mark the two places where a shared
// property is genuinely reached by different means, and each one carries why.
package proposalstoretest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/dualcontrol/proposalstore"
)

// Store is the act-neutral shape of a proposal store. Each act writes a small
// adapter onto its own store — the adapters are where the differing signatures
// (an announcement argument, an approver the act does not record) are absorbed,
// and they are three lines each.
type Store[T any] interface {
	// Put holds a proposal, or returns proposalstore.ErrExists if the id is held.
	Put(ctx context.Context, p T) error
	// Get reads without consuming.
	Get(ctx context.Context, id string) (T, bool, error)
	// Claim decides the proposal on behalf of approver and reports whether THIS
	// caller got it. An act that discards on claim ignores approver and at; see
	// Harness.RefusesSelfApprovalAtTheStore.
	Claim(ctx context.Context, id, approver string, at time.Time) (bool, error)
	// Pending is the approver's queue: still awaiting a signature at now.
	Pending(ctx context.Context, now time.Time) ([]T, error)
	// Expired is the OTHER queue: past its deadline, unsigned, and not yet
	// disposed of by whatever this act does with an expiry.
	Expired(ctx context.Context, now time.Time) ([]T, error)
}

// Harness adapts one act's store to the contract.
type Harness[T proposalstore.Proposal[T]] struct {
	// New returns an EMPTY store. It is called once per case, so a Postgres
	// harness truncates or namespaces here.
	New func(t *testing.T) Store[T]

	// Build returns a well-formed proposal for id, proposed by proposer, created
	// at at. It must be approvable: a Build that returns something the store
	// refuses turns every case below into a vacuous pass.
	Build func(t *testing.T, id, proposer string, at time.Time) T

	// RefusesSelfApprovalAtTheStore says whether Claim itself can tell that the
	// approver is the proposer.
	//
	// FALSE IS NOT A GAP TO CLOSE. datamaster's Claim takes no approver because
	// it DISCARDS the row: the durable record of who approved is the
	// append-only exception_overrides row, its handler refuses the self-approval
	// through dualcontrol.Approve before claiming, and its own CHECK refuses it
	// again at the row that matters. The OMS keeps the row, so its Claim is the
	// last line before the record an auditor reads and has to refuse there.
	RefusesSelfApprovalAtTheStore bool
}

// Run executes the contract. Every message names the operational consequence
// rather than the assertion, because the reader of a failure here is somebody
// deciding whether a dual-control surface is safe to route.
func Run[T proposalstore.Proposal[T]](t *testing.T, h Harness[T]) {
	t.Helper()

	born := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	t.Run("the dual-control record survives the round trip", func(t *testing.T) {
		s := h.New(t)
		ctx := context.Background()
		p := h.Build(t, "p-read", "user:alice@kanz", born)
		if err := s.Put(ctx, p); err != nil {
			t.Fatalf("Put: %v", err)
		}
		got, ok, err := s.Get(ctx, "p-read")
		if err != nil || !ok {
			t.Fatalf("Get: ok=%v err=%v", ok, err)
		}
		want, have := p.Record(), got.Record()
		if have.Proposer != want.Proposer || have.Digest != want.Digest ||
			have.Act != want.Act || have.Subject != want.Subject {
			t.Errorf("the round trip lost the record: stored %+v, read back %+v.\n\n"+
				"An approval is checked against these four. A proposer that does not "+
				"survive makes the self-approval check compare the wrong name; a digest "+
				"that does not survive makes every approval fail as a payload change "+
				"against a payload nobody edited", want, have)
		}
		if !have.ExpiresAt.Equal(want.ExpiresAt) {
			t.Errorf("expiry = %v, want %v — the approver's remaining window is decided by "+
				"this value, and a store that rounds or drops it either kills a live "+
				"proposal or resurrects a dead one", have.ExpiresAt, want.ExpiresAt)
		}
	})

	t.Run("an unknown proposal is not an error", func(t *testing.T) {
		s := h.New(t)
		if _, ok, err := s.Get(context.Background(), "never-existed"); err != nil || ok {
			t.Fatalf("Get of an unknown id: ok=%v err=%v — it never existed, it was already "+
				"decided, or another tenant owns it, and the three must stay "+
				"indistinguishable", ok, err)
		}
	})

	t.Run("reading a proposal does not consume it", func(t *testing.T) {
		s := h.New(t)
		ctx := context.Background()
		if err := s.Put(ctx, h.Build(t, "p-peek", "user:alice@kanz", born)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		for i := 0; i < 3; i++ {
			if _, ok, err := s.Get(ctx, "p-peek"); err != nil || !ok {
				t.Fatalf("Get %d: ok=%v err=%v — a check that has to REFUSE without "+
					"destroying the proposal (a self-approval must leave it pending for "+
					"somebody who may actually sign it) cannot use a read that consumes",
					i, ok, err)
			}
		}
	})

	t.Run("a second proposal for one id is refused and the first survives", func(t *testing.T) {
		s := h.New(t)
		ctx := context.Background()
		if err := s.Put(ctx, h.Build(t, "p-dup", "user:alice@kanz", born)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		err := s.Put(ctx, h.Build(t, "p-dup", "user:mallory@kanz", born))
		if err == nil {
			t.Fatal("a second proposal for one id was accepted — a redelivered command puts " +
				"the same decision in the approver's queue twice, and the second one can " +
				"carry a different proposer")
		}
		if !errors.Is(err, proposalstore.ErrExists) {
			t.Errorf("err = %v, want proposalstore.ErrExists — a redelivery has not FAILED, "+
				"the first delivery already did the work, and a caller that cannot tell "+
				"the two apart either nacks forever or swallows a real write failure", err)
		}
		got, ok, err := s.Get(ctx, "p-dup")
		if err != nil || !ok {
			t.Fatalf("Get after the refused duplicate: ok=%v err=%v", ok, err)
		}
		if got.Record().Proposer != "user:alice@kanz" {
			t.Errorf("proposer = %q, want the FIRST proposer — the duplicate replaced the "+
				"person the approver is being asked to differ from, which is a "+
				"self-approval one redelivery away", got.Record().Proposer)
		}
	})

	t.Run("claim elects exactly one caller", func(t *testing.T) {
		s := h.New(t)
		ctx := context.Background()
		if err := s.Put(ctx, h.Build(t, "p-claim", "user:alice@kanz", born)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		won, err := s.Claim(ctx, "p-claim", "user:bob@kanz", born.Add(time.Minute))
		if err != nil || !won {
			t.Fatalf("Claim: won=%v err=%v — nobody could act on a live proposal", won, err)
		}
		// THE PROPERTY IS NOT "Claim RETURNS TRUE", it is "exactly one caller is
		// told it won". Two approvers pass every check independently — they are
		// different people, the digest matches for both, neither has expired —
		// and both would apply.
		again, err := s.Claim(ctx, "p-claim", "user:carol@kanz", born.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("second Claim: %v", err)
		}
		if again {
			t.Fatal("a SECOND approver was also told it won — one decision is applied twice: " +
				"two override records for one exception, or two deliveries of one order " +
				"reaching a live venue")
		}

		// AND THE STORE SAYS SO, whichever way this act says it. The two shipped
		// dispositions are both correct and neither is negotiable: datamaster's row
		// is GONE (its durable record is the append-only exception_overrides row
		// carrying both names, and a second copy here could disagree with it), the
		// OMS's row REMAINS carrying the approver (an admitted order does not record
		// who approved it, so that row is the only evidence two people signed).
		//
		// WHAT NEITHER ACT MAY BE is a store where the row survives and still calls
		// itself undecided. That is the state that puts a signed proposal back on the
		// approver's queue and into the expiry sweep, and it is the one a claim that
		// half-wrote its decision would leave behind.
		got, ok, err := s.Get(ctx, "p-claim")
		if err != nil {
			t.Fatalf("Get after the claim: %v", err)
		}
		if ok && !got.Decided() {
			t.Fatal("the claimed proposal is still held AND still reports itself undecided — " +
				"an act that keeps the row must record the second signature on it, or the " +
				"proposal returns to the queue as work somebody already did")
		}
	})

	t.Run("a claimed proposal is no longer offered as work", func(t *testing.T) {
		s := h.New(t)
		ctx := context.Background()
		for _, id := range []string{"p-a", "p-b", "p-c"} {
			if err := s.Put(ctx, h.Build(t, id, "user:alice@kanz", born)); err != nil {
				t.Fatalf("Put %s: %v", id, err)
			}
		}
		if won, err := s.Claim(ctx, "p-b", "user:bob@kanz", born.Add(time.Minute)); err != nil || !won {
			t.Fatalf("Claim: won=%v err=%v", won, err)
		}
		got, err := s.Pending(ctx, born.Add(time.Hour))
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if names := ids(got); len(names) != 2 || names[0] != "p-a" || names[1] != "p-c" {
			t.Fatalf("pending = %v, want [p-a p-c] in creation order.\n\n"+
				"A DECIDED proposal on the queue is work an approver keeps being shown "+
				"after somebody did it. An order that reverses between two reads reads "+
				"as activity to whoever is watching the queue", names)
		}
	})

	t.Run("an expired proposal leaves the approver's queue", func(t *testing.T) {
		s := h.New(t)
		ctx := context.Background()
		p := h.Build(t, "p-exp", "user:alice@kanz", born)
		if err := s.Put(ctx, p); err != nil {
			t.Fatalf("Put: %v", err)
		}
		after := p.Record().ExpiresAt.Add(time.Minute)
		got, err := s.Pending(ctx, after)
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("an expired proposal is still offered as work: %v — no approval can "+
				"apply it, so the queue is inviting a signature that will be refused, and "+
				"a proposal resting there looks like a decision that was made", ids(got))
		}
	})

	// #563, AND THE REASON THIS CASE IS SHARED. datamaster's Pending filtered an
	// expired proposal out and nothing looked at those rows again, so "nobody
	// signed it" was indistinguishable from "it never existed". The OMS had the
	// same hole (#539) and closed it with an announcement; datamaster closed it
	// with a list. WHAT they do differs; that a lapsed proposal is DISCOVERABLE
	// does not.
	t.Run("an expired proposal is discoverable rather than silent", func(t *testing.T) {
		s := h.New(t)
		ctx := context.Background()
		p := h.Build(t, "p-lapsed", "user:alice@kanz", born)
		if err := s.Put(ctx, p); err != nil {
			t.Fatalf("Put: %v", err)
		}
		after := p.Record().ExpiresAt.Add(time.Minute)
		got, err := s.Expired(ctx, after)
		if err != nil {
			t.Fatalf("Expired: %v", err)
		}
		if names := ids(got); len(names) != 1 || names[0] != "p-lapsed" {
			t.Fatalf("expiry queue = %v, want [p-lapsed].\n\n"+
				"A proposal that expired unsigned is now in NO queue at all: the proposer "+
				"is left inferring the outcome from an absence, which is the silent drop "+
				"every control here exists to end", names)
		}
	})

	t.Run("a live proposal is not treated as expired", func(t *testing.T) {
		s := h.New(t)
		ctx := context.Background()
		p := h.Build(t, "p-live", "user:alice@kanz", born)
		if err := s.Put(ctx, p); err != nil {
			t.Fatalf("Put: %v", err)
		}
		got, err := s.Expired(ctx, p.Record().ExpiresAt.Add(-time.Minute))
		if err != nil {
			t.Fatalf("Expired: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("a live proposal is queued as expired: %v — the approver still had "+
				"time and whatever this act does with an expiry is about to be done to a "+
				"proposal somebody could still sign", ids(got))
		}
	})

	// AN APPROVED PROPOSAL NEVER EXPIRES. It was decided; the deadline stopped
	// mattering the moment somebody signed it. Announcing or purging one as
	// lapsed says the act was abandoned when it was in fact carried out.
	t.Run("a claimed proposal never enters the expiry queue", func(t *testing.T) {
		s := h.New(t)
		ctx := context.Background()
		p := h.Build(t, "p-signed", "user:alice@kanz", born)
		if err := s.Put(ctx, p); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if won, err := s.Claim(ctx, "p-signed", "user:bob@kanz", born.Add(time.Minute)); err != nil || !won {
			t.Fatalf("Claim: won=%v err=%v", won, err)
		}
		got, err := s.Expired(ctx, p.Record().ExpiresAt.Add(time.Hour))
		if err != nil {
			t.Fatalf("Expired: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("a SIGNED proposal is queued as expired: %v — the estate is about to "+
				"be told an act that was carried out was abandoned", ids(got))
		}
	})

	// EVERY BACKEND REFUSES THE SAME MALFORMED RECORD. Each act's Postgres schema
	// refuses most of these again at a CHECK, but a constraint violation is an
	// error indistinguishable from a broken migration, and the in-process backend
	// has no CHECK at all — so a store that only leaned on the engine accepted in
	// tests what production refuses.
	t.Run("a malformed record is refused rather than stored", func(t *testing.T) {
		base := h.Build(t, "p-bad", "user:alice@kanz", born)
		for _, c := range []struct {
			name string
			why  string
			edit func(dualcontrol.Proposal) dualcontrol.Proposal
		}{
			{"no proposer", "every approver differs from \"\", so the self-approval check " +
				"passes vacuously and one person holds both signatures while the trail shows two",
				func(r dualcontrol.Proposal) dualcontrol.Proposal { r.Proposer = ""; return r }},
			{"blank proposer", "the same vacuous check, spelled with spaces",
				func(r dualcontrol.Proposal) dualcontrol.Proposal { r.Proposer = "   "; return r }},
			{"no digest", "the approval would cover nothing, so the payload can change after " +
				"the second signature",
				func(r dualcontrol.Proposal) dualcontrol.Proposal { r.Digest = ""; return r }},
			{"no act", "an approval for ANY act would cover it",
				func(r dualcontrol.Proposal) dualcontrol.Proposal { r.Act = ""; return r }},
			{"no expiry", "unknown is not forever — an approval collected today would apply " +
				"against next quarter's book",
				func(r dualcontrol.Proposal) dualcontrol.Proposal {
					r.ExpiresAt = time.Time{}
					return r
				}},
			{"born expired", "it can never be approved and rests in the queue looking like " +
				"work nobody did",
				func(r dualcontrol.Proposal) dualcontrol.Proposal {
					r.ExpiresAt = r.CreatedAt
					return r
				}},
		} {
			t.Run(c.name, func(t *testing.T) {
				s := h.New(t)
				bad := base.WithRecord(c.edit(base.Record()))
				if err := s.Put(context.Background(), bad); err == nil {
					t.Errorf("stored a proposal with %s — %s", c.name, c.why)
				}
			})
		}
	})

	if h.RefusesSelfApprovalAtTheStore {
		// CASE AND SPACE FOLDED. One person holding both signatures by
		// capitalising a letter is the clause that fails QUIETLY: the trail then
		// shows two distinct actors, which reads as satisfied to an auditor.
		t.Run("a self-approval is refused and leaves the proposal claimable", func(t *testing.T) {
			s := h.New(t)
			ctx := context.Background()
			if err := s.Put(ctx, h.Build(t, "p-self", "user:alice@kanz", born)); err != nil {
				t.Fatalf("Put: %v", err)
			}
			for _, spelling := range []string{"user:alice@kanz", "User:Alice@Kanz", "  user:alice@kanz  "} {
				won, err := s.Claim(ctx, "p-self", spelling, born.Add(time.Minute))
				if won {
					t.Fatalf("%q approved what it proposed — the audit trail shows two names "+
						"for one person, which reads as satisfied", spelling)
				}
				_ = err // a refusal and a plain false are both acceptable; a win is not
			}
			if _, ok, err := s.Get(ctx, "p-self"); err != nil || !ok {
				t.Fatalf("a refused self-approval consumed the proposal (ok=%v err=%v) — one "+
					"person can destroy a colleague's pending decision by attempting their "+
					"own approval and being refused", ok, err)
			}
			if won, err := s.Claim(ctx, "p-self", "user:bob@kanz", born.Add(2*time.Minute)); err != nil || !won {
				t.Fatalf("a legitimate approver could not claim after a refused self-approval "+
					"(won=%v err=%v)", won, err)
			}
		})
	}

	t.Run("an unnamed approver cannot claim", func(t *testing.T) {
		s := h.New(t)
		ctx := context.Background()
		if err := s.Put(ctx, h.Build(t, "p-anon", "user:alice@kanz", born)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if !h.RefusesSelfApprovalAtTheStore {
			// The act does not learn who is claiming, so it cannot refuse an
			// unnamed one; its handler does, before it gets here. Asserting it
			// would be asserting a property this store cannot have.
			t.Skip("this act's Claim takes no approver — see Harness.RefusesSelfApprovalAtTheStore")
		}
		won, err := s.Claim(ctx, "p-anon", "", born.Add(time.Minute))
		if won {
			t.Fatal("an UNNAMED approver claimed — the audit row says two people signed " +
				"while naming one")
		}
		if err == nil {
			t.Error("an empty approver was refused SILENTLY; an approval must come from an " +
				"authenticated subject and saying so is the whole point")
		}
	})
}

func ids[T proposalstore.Proposal[T]](ps []T) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Record().ID
	}
	return out
}
