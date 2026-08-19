package proposalstore

// THE PRIMITIVES, DIRECTLY.
//
// The act-neutral contract in proposalstoretest exercises these through a real
// act's store, which is the right place for anything an act can express. Three
// properties cannot be reached that way and are the ones a subtle change would
// break silently:
//
//  1. Insert's fallible commit ordering. The OMS enqueues the FACT announcing a
//     held order there; the act's contract has no announcement to make, so a
//     store that wrote the map first and announced afterwards would pass every
//     case above while leaving an order held and announced to nobody.
//  2. Update refusing to CREATE. An approval is a release, never an origin: a
//     subject holding only the approve authority must not be able to bring a
//     proposal into existence by attempting to decide one.
//  3. Copy-on-read. A caller that can mutate what the store holds makes the
//     digest cover a value the store no longer has, and the failure appears at
//     approval time as a payload change nobody made.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dualcontrol"
)

var t0 = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// probe is a minimal act: a dual-control record plus one mutable payload, so
// copy-on-read is observable.
type probe struct {
	dualcontrol.Proposal
	Note     *string
	Approver string
}

func (p probe) Record() dualcontrol.Proposal { return p.Proposal }

func (p probe) WithRecord(r dualcontrol.Proposal) probe { p.Proposal = r; return p }

func (p probe) Decided() bool { return p.Approver != "" }

func (p probe) Copy() probe {
	if p.Note != nil {
		note := *p.Note
		p.Note = &note
	}
	return p
}

func (p probe) Validate() error { return Wellformed(p.Proposal) }

func newProbe(t *testing.T, id string, at time.Time) probe {
	t.Helper()
	base, err := dualcontrol.Propose(id, dualcontrol.ActPricingOverride, "subject:"+id,
		"user:alice@kanz", "digest-"+id, at, dualcontrol.DefaultTTL)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	note := "as proposed"
	return probe{Proposal: base, Note: &note}
}

// A FAILING COMMIT LEAVES NOTHING HELD. The commit is the announcement, and the
// order it runs in is the whole of #292 restated in memory: a proposal held
// without its FACT is a hold the estate was never told about, and the proposer
// sees neither an acceptance nor a rejection nor a pending notice.
func TestAFailingCommitHoldsNothing(t *testing.T) {
	m := NewMemory[probe]()
	ctx := context.Background()
	boom := errors.New("the queue refused the record")

	err := m.Insert(ctx, newProbe(t, "p1", t0), func() error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("Insert: %v, want the commit's own error — a caller told the write "+
			"succeeded would never retry it", err)
	}
	if m.Len() != 0 {
		t.Fatal("the proposal is HELD although its announcement was refused — it is now " +
			"awaiting a signature nobody has been told to give")
	}
	if _, ok, err := m.Get(ctx, "p1"); err != nil || ok {
		t.Fatalf("Get after a failed commit: ok=%v err=%v", ok, err)
	}
}

// A REFUSED DUPLICATE COMMITS NOTHING. Otherwise a redelivered command announces
// the same hold once per delivery, and the approver's queue fills with one
// decision repeated.
func TestARefusedDuplicateCommitsNothing(t *testing.T) {
	m := NewMemory[probe]()
	ctx := context.Background()
	if err := m.Insert(ctx, newProbe(t, "p1", t0), nil); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	commits := 0
	err := m.Insert(ctx, newProbe(t, "p1", t0), func() error { commits++; return nil })
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second Insert: %v, want ErrExists", err)
	}
	if commits != 0 {
		t.Errorf("the refused duplicate ran its commit %d time(s) — the hold is announced "+
			"once per redelivery, forever", commits)
	}
}

// UPDATE DECIDES; IT NEVER ORIGINATES. An approval is a release, so a subject
// holding only the approve authority must not be able to create the proposal it
// then approves.
func TestUpdateOnAnUnknownIdCreatesNothing(t *testing.T) {
	m := NewMemory[probe]()
	ctx := context.Background()
	called := false
	won, err := m.Update(ctx, "never-held", func(p probe) (probe, bool, error) {
		called = true
		p.Approver = "user:bob@kanz"
		return p, true, nil
	})
	if err != nil || won {
		t.Fatalf("Update on an unknown id: won=%v err=%v", won, err)
	}
	if called {
		t.Error("the decide function ran for an id nothing holds — it was handed a zero " +
			"proposal, which has no proposer, so every approver differs from it")
	}
	if m.Len() != 0 {
		t.Fatal("deciding a proposal that was never held CREATED one — the approve authority " +
			"can now originate the acts it is only supposed to release")
	}
}

// A DECIDE THAT ERRORS WRITES NOTHING, and the proposal stays exactly as it was.
// This is the in-process stand-in for the Postgres rollback: the OMS's Claim
// refuses a self-approval from inside decide, and the proposal must be left
// pending for somebody who may legitimately sign it.
func TestADecideThatErrorsLeavesTheProposalUntouched(t *testing.T) {
	m := NewMemory[probe]()
	ctx := context.Background()
	if err := m.Insert(ctx, newProbe(t, "p1", t0), nil); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	refused := errors.New("refused")
	won, err := m.Update(ctx, "p1", func(p probe) (probe, bool, error) {
		p.Approver = "user:alice@kanz"
		note := "tampered"
		p.Note = &note
		return p, true, refused
	})
	if won {
		t.Fatal("a refused decision reported that it won")
	}
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the decide function's own error — an operator staring at a "+
			"refused approval needs to know which rule refused it", err)
	}
	got, ok, err := m.Get(ctx, "p1")
	if err != nil || !ok {
		t.Fatalf("Get after a refused decision: ok=%v err=%v — the refusal CONSUMED the "+
			"proposal, so one person can destroy a colleague's pending decision by "+
			"attempting their own and being refused", ok, err)
	}
	if got.Approver != "" || *got.Note != "as proposed" {
		t.Errorf("the refused decision was written anyway: approver=%q note=%q",
			got.Approver, *got.Note)
	}
}

// COPY-ON-READ, IN BOTH DIRECTIONS. A stored proposal that shares memory with
// the caller is not a record of what was proposed: the digest an approver signs
// would cover a value the store no longer holds, and the mismatch surfaces at
// approval time as a payload change nobody made.
func TestTheStoreDoesNotShareMemoryWithItsCallers(t *testing.T) {
	m := NewMemory[probe]()
	ctx := context.Background()
	p := newProbe(t, "p1", t0)
	if err := m.Insert(ctx, p, nil); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// THE CALLER'S ORIGINAL. It still holds the pointer it passed in.
	*p.Note = "changed after proposing"
	got, _, _ := m.Get(ctx, "p1")
	if *got.Note != "as proposed" {
		t.Errorf("the proposer edited the stored proposal after proposing it (note = %q) — "+
			"the approval covers the digest of the original and would be applied to this",
			*got.Note)
	}

	// AND THE VALUE HANDED OUT.
	*got.Note = "changed after reading"
	again, _, _ := m.Get(ctx, "p1")
	if *again.Note != "as proposed" {
		t.Errorf("a reader edited the stored proposal (note = %q)", *again.Note)
	}

	// INCLUDING THROUGH A LIST.
	listed, err := m.Select(ctx, func(probe) bool { return true })
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	*listed[0].Note = "changed after listing"
	final, _, _ := m.Get(ctx, "p1")
	if *final.Note != "as proposed" {
		t.Errorf("a queue reader edited the stored proposal (note = %q)", *final.Note)
	}
}

// THE ORDER IS TOTAL. Two proposals created in the same nanosecond must not list
// in whichever order the map yielded: a pending queue that reorders between two
// reads reads as activity to whoever is watching it, and nobody can tell a
// reshuffle from a new proposal.
func TestTheListingOrderIsTotalAndStable(t *testing.T) {
	ctx := context.Background()
	for attempt := 0; attempt < 20; attempt++ {
		m := NewMemory[probe]()
		for _, id := range []string{"c", "a", "b"} {
			if err := m.Insert(ctx, newProbe(t, id, t0), nil); err != nil {
				t.Fatalf("Insert %s: %v", id, err)
			}
		}
		// One later proposal, to prove created_at outranks the id tiebreak.
		if err := m.Insert(ctx, newProbe(t, "aa", t0.Add(-time.Hour)), nil); err != nil {
			t.Fatalf("Insert aa: %v", err)
		}
		got, err := m.Select(ctx, func(probe) bool { return true })
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		var order []string
		for _, p := range got {
			order = append(order, p.ID)
		}
		if strings.Join(order, ",") != "aa,a,b,c" {
			t.Fatalf("attempt %d: order = %v, want [aa a b c] — oldest first, ties broken by id",
				attempt, order)
		}
	}
}

// PURGE IS WHAT BOUNDS THE TABLE. A proposal nobody signs is never claimed, so
// without this every proposal ever left unsigned stays forever — and it must
// take exactly what the predicate names and nothing else.
func TestPurgeTakesOnlyWhatThePredicateNames(t *testing.T) {
	m := NewMemory[probe]()
	ctx := context.Background()
	old := newProbe(t, "old", t0.Add(-48*time.Hour))
	live := newProbe(t, "live", t0)
	for _, p := range []probe{old, live} {
		if err := m.Insert(ctx, p, nil); err != nil {
			t.Fatalf("Insert %s: %v", p.ID, err)
		}
	}
	n, err := m.Purge(ctx, func(p probe) bool { return Expired(p.Proposal, t0) })
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d, want 1", n)
	}
	if _, ok, _ := m.Get(ctx, "live"); !ok {
		t.Fatal("the purge took a LIVE proposal — somebody still had time to sign it")
	}
	if _, ok, _ := m.Get(ctx, "old"); ok {
		t.Fatal("the purge reported a row it did not take, so the table is unbounded and " +
			"the metric says otherwise")
	}
}

// EXPIRY IS INCLUSIVE OF THE INSTANT, and it asks about the deadline alone.
//
// The boundary is the half that matters: dualcontrol.Approve refuses an approval
// at ExpiresAt exactly, so a store that called the same instant "not yet
// expired" leaves one second in which an approval is accepted for a proposal no
// sweeper will ever find — and one in which the sweeper announces a proposal
// somebody is being told they may still sign.
func TestExpiryIsInclusiveOfTheInstantAndAsksOnlyAboutTheDeadline(t *testing.T) {
	p := newProbe(t, "p1", t0)

	if Expired(p.Proposal, p.ExpiresAt.Add(-time.Nanosecond)) {
		t.Error("a proposal one nanosecond before its deadline reports as expired — an " +
			"approver with time left is told their signature came too late")
	}
	if !Expired(p.Proposal, p.ExpiresAt) {
		t.Error("at the deadline exactly, the proposal is NOT expired here, while " +
			"dualcontrol.Approve already refuses an approval at that instant. That second " +
			"is one where nothing can approve the proposal and nothing will sweep it")
	}
	if !Expired(p.Proposal, p.ExpiresAt.Add(time.Nanosecond)) {
		t.Error("past the deadline, the proposal is not expired")
	}

	// A SIGNED PROPOSAL PAST ITS DEADLINE IS STILL "EXPIRED" HERE. That is
	// deliberate: this asks about the clock, and each act adds its own clause for
	// "and nobody signed it". An act that forgot the second clause would announce
	// a carried-out act as abandoned, which is why the acts' own contracts assert
	// a claimed proposal never enters the expiry queue.
	p.Approver = "user:bob@kanz"
	if !Expired(p.Proposal, p.ExpiresAt.Add(time.Hour)) {
		t.Error("Expired consulted the approver — it must answer about the deadline alone, " +
			"or an act cannot tell the two questions apart")
	}
}
