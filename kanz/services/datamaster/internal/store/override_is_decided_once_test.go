package store

// ONE OPERATOR DECISION LEAVES ONE AUDIT ROW AND ONE FACT (#816).
//
// PostgresExceptions.Override selected `status` under FOR UPDATE and then never
// referenced it. Every statement after the lock therefore ran unconditionally: a
// second override on an already-OVERRIDDEN exception appended a second
// exception_overrides row, re-flipped the status, and enqueued a second FACT
// through the outbox. The two FACTs carry different event ids, so audit's dedup
// cannot collapse them, and the append-only trail this control exists to produce
// ends up holding two authorisations for one human decision. "Who overrode this
// price, and when" then has two answers, which for an attribution control is the
// failure.
//
// # WHY THE DEFECT IS NON-IDEMPOTENCY AND NOT A RACE
//
// The lock is correct and always was. FOR UPDATE serialises two concurrent
// overrides of the same exception perfectly well — TwoConcurrentOverrides below
// is the arm that says so, and it passes for the lock's sake, not the check's.
// What was missing is that the operation was not IDEMPOTENT: a single client
// retrying after a timeout whose first attempt had in fact committed produced
// the same duplicate as two callers would, with no concurrency involved at all.
// A value read under a lock and never branched on is a lock taken for a decision
// nobody made.
//
// # WHY THESE TESTS NEED A REAL DATABASE
//
// Three of the four properties live in Postgres and nowhere else: the row count
// in exception_overrides, the outbox row the FACT rides in, and the rollback that
// keeps a dual-control proposal unspent when the refusal fires after the claim.
// The in-memory backend has no outbox and no transaction, so it can host the
// refusal (runExceptionContract asserts it there) but not the two things the
// issue's "verified when" actually names.
//
// They skip without TEST_POSTGRES_URL, like the fifteen others in this package.
// Skipping is not passing.

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/services/datamaster/internal/pricing"
)

// overrideRowCount is how many audit rows exist for one exception. It is the
// count the issue's "verified when" names, read from the table rather than
// through Get, so a read path that deduplicated would not hide a duplicate write.
func overrideRowCount(t *testing.T, pool *pgxpool.Pool, exceptionID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM exception_overrides WHERE exception_id = $1`, exceptionID).Scan(&n); err != nil {
		t.Fatalf("count exception_overrides: %v", err)
	}
	return n
}

// A REPEATED SINGLE-SIGNED OVERRIDE WRITES NO SECOND ROW AND NO SECOND FACT.
//
// #816's "verified when", reproduced verbatim. The unarmed path is the one with
// no protection at all: a proposal is single-use, so maker-checker made the
// dual-signed path idempotent as a side effect, and
// DATAMASTER_REQUIRE_DUAL_CONTROL defaults to false — so this is the path
// production is on today.
func TestPostgresOverride_ASecondOverrideWritesNoRowAndNoFACT(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	es := NewPostgresExceptions(pool, "__system__")
	const exID = "ONCE:PRICE_TOLERANCE:ICE"
	seedException(t, pool, exID)

	first, _ := new(big.Rat).SetString("130")
	if err := es.Override(ctx, exID, pricing.Override{
		Actor: "alice@kanz", Reason: "vendor confirmed", ChosenPrice: first, At: pgNow,
	}, Claim{}); err != nil {
		t.Fatalf("the first override was refused: %v", err)
	}
	afterFirst := outboxCount(t, pool)
	if got := overrideRowCount(t, pool, exID); got != 1 {
		t.Fatalf("%d audit row(s) after ONE override, want 1", got)
	}

	// THE SECOND DECISION, by a different person and at a different price. Both
	// halves matter: it is not a duplicate request, and it is still refused —
	// the exception has been decided, and a second authorisation for one act is
	// what the trail must not hold.
	second, _ := new(big.Rat).SetString("160")
	err := es.Override(ctx, exID, pricing.Override{
		Actor: "bob@kanz", Reason: "re-reviewed", ChosenPrice: second, At: pgNow,
	}, Claim{})
	if !errors.Is(err, pricing.ErrAlreadyOverridden) {
		t.Fatalf("a second override on an OVERRIDDEN exception returned %v, want "+
			"pricing.ErrAlreadyOverridden", err)
	}

	if got := overrideRowCount(t, pool, exID); got != 1 {
		t.Errorf("exception_overrides holds %d rows for ONE operator decision, want 1. The audit "+
			"trail now shows two authorisations where one human decided, and an examiner asking "+
			"who overrode this price gets two answers", got)
	}
	if after := outboxCount(t, pool); after != afterFirst {
		t.Errorf("the refused override enqueued %d FACT(s). Each carries its own event id, so "+
			"audit's dedup cannot collapse them and the estate hears one decision announced "+
			"twice", after-afterFirst)
	}
	// AND THE FIRST DECISION IS INTACT. A refusal that quietly replaced the
	// stored record would keep the row count at one and still lose alice's
	// signature, which is the same corruption wearing the passing count.
	ex, ok, err := es.Get(ctx, exID)
	if err != nil || !ok {
		t.Fatalf("read the exception back: ok=%v err=%v", ok, err)
	}
	if len(ex.Overrides) != 1 || ex.Overrides[0].Actor != "alice@kanz" {
		t.Fatalf("the trail after the refusal is %+v, want alice@kanz's single record", ex.Overrides)
	}
	if ex.Overrides[0].ChosenPrice.Cmp(first) != 0 {
		t.Errorf("the recorded price is %s, want %s — the refused decision displaced the stored one",
			ex.Overrides[0].ChosenPrice.RatString(), first.RatString())
	}
	if ex.Status != pricing.StatusOverridden {
		t.Errorf("status = %s after a refused second override, want OVERRIDDEN — the refusal "+
			"disturbed the state it was protecting", ex.Status)
	}
}

// AN APPROVAL FOR AN ALREADY-DECIDED EXCEPTION LEAVES ITS PROPOSAL UNSPENT.
//
// The refusal fires AFTER claimProposal, because the claim is deliberately the
// first statement in the transaction (#807 — a racing approver blocks on the
// proposal row before it takes the exception's, so this transaction never waits
// on one lock while holding another). That ordering is only safe because the
// refusal returns before COMMIT and the deferred rollback takes the claim with
// it. If it did not, this control would spend a second signature on an act that
// never happened — the exact cost #807 exists to prevent, reintroduced by the
// fix for #816.
//
// The proposal then stays pending and lapses visibly on
// GET /v1/exceptions/pending-overrides (#563), rather than disappearing.
func TestPostgresOverride_AnApprovalForADecidedExceptionKeepsItsProposal(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	es := NewPostgresExceptions(pool, "__system__")
	ps := NewPostgresProposals(pool)
	const exID = "DECIDED:PRICE_TOLERANCE:ICE"
	seedException(t, pool, exID)

	price, _ := new(big.Rat).SetString("130")
	if err := es.Override(ctx, exID, pricing.Override{
		Actor: "alice@kanz", Reason: "vendor confirmed", ChosenPrice: price, At: pgNow,
	}, Claim{}); err != nil {
		t.Fatalf("the first override was refused: %v", err)
	}
	afterFirst := outboxCount(t, pool)

	// A proposal raised against an exception that is already decided — the state
	// nothing prevents, because proposing does not touch the exception.
	prop := proposalFor(t, "prop-decided", exID, "carol@kanz", "160")
	if err := ps.Put(ctx, prop); err != nil {
		t.Fatalf("put proposal: %v", err)
	}

	err := es.Override(ctx, exID, prop.ApplyOverride("dave@kanz", pgNow), Claim{ProposalID: prop.ID})
	if !errors.Is(err, pricing.ErrAlreadyOverridden) {
		t.Fatalf("approving an override for an already-decided exception returned %v, want "+
			"pricing.ErrAlreadyOverridden", err)
	}

	got, found, err := ps.Get(ctx, prop.ID)
	if err != nil {
		t.Fatalf("read the proposal back: %v", err)
	}
	if !found {
		t.Fatal("the refusal consumed the proposal. The second signature is spent on an override " +
			"that did not happen, and nothing records that an approval was given — #807's cost, " +
			"arriving through #816's refusal. The claim must roll back with the rest.")
	}
	if got.Proposer != "carol@kanz" {
		t.Errorf("the surviving proposal is not the one that was made: proposer=%q", got.Proposer)
	}
	if n := overrideRowCount(t, pool, exID); n != 1 {
		t.Errorf("%d audit rows after the refused approval, want 1", n)
	}
	if after := outboxCount(t, pool); after != afterFirst {
		t.Errorf("the refused approval enqueued %d FACT(s), want 0", after-afterFirst)
	}
}

// TWO CONCURRENT OVERRIDES OF ONE OPEN EXCEPTION: EXACTLY ONE APPLIES.
//
// This is the arm that keeps the RCA honest. The FOR UPDATE was never the broken
// part, and this proves the check does not rest on the lock being wrong: two
// single-signed overrides, no proposal between them, both racing an exception
// that is genuinely OPEN. One wins; the other blocks on the row lock, reads
// OVERRIDDEN once the winner commits, and is refused with nothing written.
//
// Before the fix BOTH would have applied — which is why this is a concurrency
// test that would not have failed for a concurrency reason.
func TestPostgresOverride_TwoConcurrentOverridesApplyOnce(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	es := NewPostgresExceptions(pool, "__system__")
	const exID = "CONC:PRICE_TOLERANCE:ICE"
	seedException(t, pool, exID)
	before := outboxCount(t, pool)

	price, _ := new(big.Rat).SetString("130")
	const racers = 4
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		applied int
		refused int
		others  []error
	)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			actor := []string{"alice@kanz", "bob@kanz", "carol@kanz", "dave@kanz"}[i]
			err := es.Override(ctx, exID, pricing.Override{
				Actor: actor, Reason: "vendor confirmed", ChosenPrice: price, At: pgNow,
			}, Claim{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				applied++
			case errors.Is(err, pricing.ErrAlreadyOverridden):
				refused++
			default:
				others = append(others, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range others {
		t.Errorf("an override failed for a reason that is neither applying nor being refused: %v", err)
	}
	if applied != 1 || refused != racers-1 {
		t.Fatalf("%d of %d concurrent overrides applied and %d were refused, want exactly 1 and %d",
			applied, racers, refused, racers-1)
	}
	if n := overrideRowCount(t, pool, exID); n != 1 {
		t.Errorf("%d audit rows for ONE applied override", n)
	}
	if after := outboxCount(t, pool); after != before+1 {
		t.Errorf("the race enqueued %d FACT(s), want exactly 1", after-before)
	}
}
