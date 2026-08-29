package store

// THE CLAIM AND THE OVERRIDE COMMIT TOGETHER, OR NEITHER DOES (#807).
//
// The approve path used to be two transactions: ProposalStore.Claim committed,
// and then ExceptionStore.Override committed. Everything between them was a
// window in which the second signature was already spent and the override had
// not happened — and the process dying there left NO record of either half. The
// proposal read as decided, the exception read as OPEN, and the only way anyone
// found out was a human noticing the contradiction and a second approver signing
// again. Until then the book was valued off a price the system itself flagged.
//
// # Why these tests need a real database
//
// The window is a COMMIT boundary, and a commit boundary has no representation
// in Go. The in-memory backend cannot host this test at all: its two halves are
// maps in one process, so the death that separates them takes both. Only
// Postgres can show a claim surviving — or not surviving — an override that
// failed after it.
//
// # What stands in for the crash
//
// A price with nine decimals. The platform's wire Decimal has a fixed scale of
// 8, so overrideEvent refuses it — and it does so at the LAST statement before
// COMMIT, with the proposal deleted, the audit row inserted and the status
// already flipped inside the transaction. That is the deepest reachable point in
// the window, and a process death there is indistinguishable from it. If the
// claim rides the transaction, all of it rolls back and the approval can be
// given again; if the claim committed on its own, the signature is gone and
// nothing was applied.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/services/datamaster/internal/pricing"
)

// outboxCount is how many FACTs are queued, so a test can assert that a rolled
// back override announced nothing.
func outboxCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// AN OVERRIDE THAT FAILS AFTER THE CLAIM LEAVES THE PROPOSAL APPROVABLE.
//
// This is #807's "verified when", reproduced: the approval must still be
// re-presentable after the transaction that would have applied it dies. Before
// the fix the claim had already committed on its own connection, so the rollback
// below could not reach it — the proposal was gone, the exception was still
// OPEN, and no row, FACT or log line recorded that a second person had signed.
func TestPostgresOverride_AFailedOverrideLeavesTheProposalApprovable(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	es := NewPostgresExceptions(pool, "__system__")
	ps := NewPostgresProposals(pool)
	const exID = "CRASH:PRICE_TOLERANCE:ICE"
	seedException(t, pool, exID)

	// NINE DECIMALS: storable, and NOT announceable — see the file header. The
	// override therefore fails at the last statement before COMMIT.
	prop := proposalFor(t, "prop-crash", exID, "alice@kanz", "130.123456789")
	if err := ps.Put(ctx, prop); err != nil {
		t.Fatalf("put proposal: %v", err)
	}
	before := outboxCount(t, pool)

	err := es.Override(ctx, exID, prop.ApplyOverride("bob@kanz", pgNow), Claim{ProposalID: prop.ID})
	if err == nil {
		t.Fatal("the override succeeded, so this test never entered the window it exists to " +
			"measure — the nine-decimal price must still be unrepresentable on the wire")
	}
	// NON-VACUITY: it must have failed for the reason that puts it deep inside
	// the transaction, not because the claim itself lost. A lost claim writes
	// nothing and would satisfy every assertion below having tested nothing.
	if errors.Is(err, ErrProposalAlreadyDecided) {
		t.Fatalf("the claim lost rather than the FACT failing, so nothing after the claim ran: %v", err)
	}

	got, found, err := ps.Get(ctx, prop.ID)
	if err != nil {
		t.Fatalf("read the proposal back: %v", err)
	}
	if !found {
		t.Fatal("THE PROPOSAL WAS CONSUMED BY AN OVERRIDE THAT DID NOT APPLY.\n\n" +
			"The second signature is spent and nothing happened: the exception stays unresolved, " +
			"the proposal that would have resolved it is gone, and no record anywhere says an " +
			"approval was given. Recovering means a human spotting the contradiction and a second " +
			"approver signing again, while the book is valued off a price the system flagged " +
			"(#807). The claim must be a statement in the override's own transaction.")
	}
	if got.Proposer != "alice@kanz" || got.ChosenPrice.Cmp(prop.ChosenPrice) != 0 {
		t.Errorf("the surviving proposal is not the one that was made: proposer=%q price=%s",
			got.Proposer, got.ChosenPrice.RatString())
	}

	ex, ok, err := es.Get(ctx, exID)
	if err != nil || !ok {
		t.Fatalf("read the exception back: ok=%v err=%v", ok, err)
	}
	if ex.Status != pricing.StatusOpen {
		t.Errorf("the exception is %s after an override that failed, want OPEN", ex.Status)
	}
	if len(ex.Overrides) != 0 {
		t.Errorf("%d override records were written by an override that failed", len(ex.Overrides))
	}
	if after := outboxCount(t, pool); after != before {
		t.Errorf("a failed override left %d FACT(s) in the outbox", after-before)
	}
}

// THE APPROVAL CONSUMES ITS PROPOSAL AND APPLIES, AS ONE COMMIT.
//
// The other direction of the same invariant, and the one that would catch a
// "fix" that simply stopped claiming: an applied override must leave nothing
// approvable behind, or the same decision can be signed twice.
func TestPostgresOverride_AnApprovalConsumesItsProposalAndApplies(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	es := NewPostgresExceptions(pool, "__system__")
	ps := NewPostgresProposals(pool)
	const exID = "APPLY:PRICE_TOLERANCE:ICE"
	seedException(t, pool, exID)

	prop := proposalFor(t, "prop-apply", exID, "alice@kanz", "130.25")
	if err := ps.Put(ctx, prop); err != nil {
		t.Fatalf("put proposal: %v", err)
	}
	before := outboxCount(t, pool)

	if err := es.Override(ctx, exID, prop.ApplyOverride("bob@kanz", pgNow), Claim{ProposalID: prop.ID}); err != nil {
		t.Fatalf("a dual-signed override was refused: %v", err)
	}

	if _, found, err := ps.Get(ctx, prop.ID); err != nil || found {
		t.Errorf("the proposal survived the override it authorised (found=%v err=%v) — the same "+
			"approval could be presented again and append a second override for one decision",
			found, err)
	}
	ex, ok, err := es.Get(ctx, exID)
	if err != nil || !ok {
		t.Fatalf("read the exception back: ok=%v err=%v", ok, err)
	}
	if ex.Status != pricing.StatusOverridden {
		t.Errorf("status = %s after an applied override, want OVERRIDDEN", ex.Status)
	}
	if len(ex.Overrides) != 1 {
		t.Fatalf("%d override records, want 1", len(ex.Overrides))
	}
	if o := ex.Overrides[0]; o.Actor != "alice@kanz" || o.Approver != "bob@kanz" {
		t.Errorf("the trail names actor=%q approver=%q — both signatures must reach it",
			o.Actor, o.Approver)
	}
	if after := outboxCount(t, pool); after != before+1 {
		t.Errorf("the override enqueued %d FACT(s), want exactly 1", after-before)
	}
}

// TWO APPROVERS RACE FOR ONE PROPOSAL AND EXACTLY ONE APPLIES.
//
// The claim moved into the override's transaction, so this is where the
// serialisation now has to hold. Both approvers pass every check — different
// people, matching digest, neither expired — and the DELETE is the only thing
// standing between them and two override records for one decision. The loser
// blocks on the winner's row lock and then finds zero rows, which is
// ErrProposalAlreadyDecided with nothing written.
//
// A mutex could not do this: the claim used to be the serialisation point on a
// separate statement, and datamaster's manifest runs replicas: 2, so an
// in-process lock says nothing about the other pod.
func TestPostgresOverride_TwoApproversRaceAndExactlyOneApplies(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	es := NewPostgresExceptions(pool, "__system__")
	ps := NewPostgresProposals(pool)
	const exID = "RACE:PRICE_TOLERANCE:ICE"
	seedException(t, pool, exID)

	prop := proposalFor(t, "prop-race-apply", exID, "alice@kanz", "130")
	if err := ps.Put(ctx, prop); err != nil {
		t.Fatalf("put proposal: %v", err)
	}
	before := outboxCount(t, pool)

	const racers = 4
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		won    int
		lost   int
		others []error
	)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Every racer is a DIFFERENT approver, so none of them is refused by
			// the self-approval rule before reaching the claim.
			approver := []string{"bob@kanz", "carol@kanz", "dave@kanz", "erin@kanz"}[i]
			err := es.Override(ctx, exID, prop.ApplyOverride(approver, pgNow), Claim{ProposalID: prop.ID})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrProposalAlreadyDecided):
				lost++
			default:
				others = append(others, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range others {
		t.Errorf("an approver failed for a reason that is neither winning nor losing the claim: %v", err)
	}
	if won != 1 || lost != racers-1 {
		t.Fatalf("%d of %d concurrent approvals applied and %d were told it was already decided, "+
			"want exactly 1 and %d — every extra winner appends another override record for one "+
			"decision", won, racers, lost, racers-1)
	}
	ex, ok, err := es.Get(ctx, exID)
	if err != nil || !ok {
		t.Fatalf("read the exception back: ok=%v err=%v", ok, err)
	}
	if len(ex.Overrides) != 1 {
		t.Errorf("%d override records for ONE decision — the audit trail double-counts an approval",
			len(ex.Overrides))
	}
	if after := outboxCount(t, pool); after != before+1 {
		t.Errorf("the race enqueued %d FACT(s), want exactly 1 — the estate would hear one decision "+
			"announced twice", after-before)
	}
}

// A CLAIM FOR A PROPOSAL NOBODY HOLDS WRITES NOTHING.
//
// The refusal has to be a refusal rather than an unclaimed override: applying it
// would put a dual-signed record in the trail for an approval this store cannot
// show was ever given.
func TestPostgresOverride_AnUnknownClaimRefusesAndWritesNothing(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	es := NewPostgresExceptions(pool, "__system__")
	const exID = "GHOST:PRICE_TOLERANCE:ICE"
	seedException(t, pool, exID)

	prop := proposalFor(t, "prop-never-stored", exID, "alice@kanz", "130")
	before := outboxCount(t, pool)

	err := es.Override(ctx, exID, prop.ApplyOverride("bob@kanz", pgNow), Claim{ProposalID: prop.ID})
	if !errors.Is(err, ErrProposalAlreadyDecided) {
		t.Fatalf("an override carrying a claim on a proposal that is not held returned %v, want "+
			"ErrProposalAlreadyDecided", err)
	}
	ex, ok, err := es.Get(ctx, exID)
	if err != nil || !ok {
		t.Fatalf("read the exception back: ok=%v err=%v", ok, err)
	}
	if ex.Status != pricing.StatusOpen || len(ex.Overrides) != 0 {
		t.Errorf("the refused override still wrote: status=%s overrides=%d", ex.Status, len(ex.Overrides))
	}
	if after := outboxCount(t, pool); after != before {
		t.Errorf("a refused override left %d FACT(s) in the outbox", after-before)
	}
}
