package order

// THE PROPOSALS TABLE, AGAINST A REAL POSTGRES (#410). Gated on
// TEST_POSTGRES_URL — they skip without a database.
//
// WHY THESE EXIST AT ALL. #495 shipped the override path's proposals table, its
// actor <> approver CHECK and its RLS policy with NO Postgres-gated test, and
// #498 recorded that as the weakest position in the change, "because none of
// them can be verified by reasoning about Go". A CHECK constraint and an RLS
// policy are exactly the things that fail SILENTLY: the constraint that is never
// violated in a passing test is indistinguishable from the constraint that was
// never created, and a policy that filters nothing looks like a tenant with no
// data.
//
// Four properties here cannot be shown by the in-memory store at all:
//
//  1. the CHECK refuses a self-approval AT THE ENGINE, so a writer that does not
//     come through Claim still cannot forge the row an auditor reads;
//  2. RLS scopes the table, so a proposal is neither readable nor APPROVABLE
//     across tenants;
//  3. two concurrent Claims elect exactly one winner ACROSS CONNECTIONS — a
//     mutex in one process says nothing about the other pod, and the shipped OMS
//     runs replicas: 2.
//  4. the same, for EXPIRY (#539) — and it is the harder half, because expiry is
//     not an act anybody performs. Nobody races an approval by accident; every
//     replica's sweeper reaches the same expired row on the same tick, with
//     nothing between them but the row. The marker 0010 adds is what makes that
//     arbitration possible, and the CHECK it carries is the engine refusing an
//     expiry announced before the deadline it claims.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/outbox"
)

// poolAs opens a second pool on the SAME test schema under a different tenant.
// tenant == "" leaves app.tenant_id unset, which is the unscoped-session case
// app_current_tenant() must RAISE on rather than answer with zero rows.
//
// It deliberately does NOT call applySchema: that drops and recreates the
// schema, and a second pool doing so mid-test would delete the rows under test.
func poolAs(t *testing.T, tenant string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run order Postgres integration tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "SET search_path TO "+testSchema); err != nil {
			return err
		}
		if tenant == "" {
			return nil
		}
		_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestPostgresProposalsHonourTheContract runs the identical assertions the
// in-memory store is held to. A seam that accepted what the engine refuses would
// certify behaviour production does not have — the fakeBus lesson, one layer
// down.
func TestPostgresProposalsHonourTheContract(t *testing.T) {
	runProposalContract(t, func(t *testing.T) ProposalStore {
		t.Helper()
		return NewPostgres(newPool(t)).Proposals()
	})
}

// TestTheDatabaseRefusesASelfApprovalEvenWhenNothingInGoDoes.
//
// Claim already excludes it in its WHERE clause, and dualcontrol.Approve refuses
// it before that. THIS TEST GOES ROUND BOTH, with raw SQL, because that is the
// threat the constraint exists for: this row is the OMS's durable evidence that
// two people signed an order, and a row where proposer = approver reads to an
// auditor as four-eyes while one person held both signatures. It is the single
// most valuable row on this table to forge and the one an auditor is least able
// to check.
func TestTheDatabaseRefusesASelfApprovalEvenWhenNothingInGoDoes(t *testing.T) {
	pool := newPool(t)
	store := NewPostgres(pool).Proposals()
	ctx := context.Background()

	if err := store.Put(ctx, heldOrder(t, "o-forge", "user:alice@kanz", t0), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// EXACTLY THE SPELLINGS THE CONTROL MUST FOLD. A byte comparison would call
	// "User:Alice@Kanz" a different person, and the trail would then show two
	// distinct actors for one — the failure #495 named as the one that fails
	// quietly.
	for _, approver := range []string{
		"user:alice@kanz",   // identical
		"User:Alice@Kanz",   // case
		" user:alice@kanz ", // surrounding space
	} {
		_, err := pool.Exec(ctx,
			`UPDATE order_proposals SET approver = $2, decided_at = now() WHERE order_id = $1`,
			"o-forge", approver)
		if err == nil {
			t.Fatalf("the database ACCEPTED approver=%q on a proposal proposed by %q — one person "+
				"holds both signatures and the audit row says four-eyes", approver, "user:alice@kanz")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("approver=%q was refused by something other than a CHECK constraint (%v) — "+
				"the refusal must come from the engine, or it is not there for a writer that "+
				"bypasses Go", approver, err)
		}
		if !strings.Contains(pgErr.ConstraintName, "approver_is_not_proposer") {
			t.Errorf("refused by constraint %q, want the self-approval one — a different constraint "+
				"passing for this is how the real one goes missing unnoticed", pgErr.ConstraintName)
		}
	}

	// AND A DIFFERENT PERSON IS STILL ACCEPTED, so the test above cannot pass
	// against a constraint that refuses every approver.
	if _, err := pool.Exec(ctx,
		`UPDATE order_proposals SET approver = $2, decided_at = now() WHERE order_id = $1`,
		"o-forge", "user:bob@kanz"); err != nil {
		t.Fatalf("the CHECK refused a genuine second signature (%v) — dual control would be "+
			"impossible to complete", err)
	}
}

// TestTheDatabaseRefusesADecisionWithNoTimestampOrNoApprover. "Approved" and
// "when" must move together: a row with an approver and no timestamp cannot be
// ordered in a trail, and one with a timestamp and no approver claims a decision
// nobody made.
func TestTheDatabaseRefusesADecisionWithNoTimestampOrNoApprover(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	if err := NewPostgres(pool).Proposals().Put(ctx, heldOrder(t, "o-half", "user:alice@kanz", t0), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, q := range []string{
		`UPDATE order_proposals SET approver = 'user:bob@kanz' WHERE order_id = 'o-half'`,
		`UPDATE order_proposals SET decided_at = now() WHERE order_id = 'o-half'`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("the database accepted a half-recorded decision: %s", q)
		}
	}
}

// TestTheDatabaseRefusesAHalfWrittenRefusal is 0012's CHECK, at the engine (#558).
//
// RecordRefusal writes the three columns together and both stores refuse a
// partial one, so this can only fail for a writer that does not come through
// there — a repair script, a future method, a migration. The row it would leave
// renders on the approver's queue as state = "refused" with nothing to show for
// it: "something happened and we cannot say what", which is the WARN this issue
// replaced, promoted to a UI.
func TestTheDatabaseRefusesAHalfWrittenRefusal(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	if err := NewPostgres(pool).Proposals().Put(ctx, heldOrder(t, "o-partial", "user:alice@kanz", t0), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, q := range []string{
		`UPDATE order_proposals SET refusal_reason = 'self_approval' WHERE order_id = 'o-partial'`,
		`UPDATE order_proposals SET refused_by = 'user:bob@kanz' WHERE order_id = 'o-partial'`,
		`UPDATE order_proposals SET refused_at = now() WHERE order_id = 'o-partial'`,
		`UPDATE order_proposals SET refusal_reason = 'expired', refused_by = 'user:bob@kanz'
		   WHERE order_id = 'o-partial'`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("the database accepted a half-written refusal: %s", q)
		}
	}
	// AND THE WHOLE ONE IS ACCEPTED. Without this the test above passes against a
	// column set nothing can ever write, which is a constraint that forbids the
	// feature rather than the defect.
	if _, err := pool.Exec(ctx, `UPDATE order_proposals
		   SET refusal_reason = 'self_approval', refused_by = 'user:alice@kanz', refused_at = now()
		 WHERE order_id = 'o-partial'`); err != nil {
		t.Fatalf("the CHECK refused a complete refusal (%v) — no refusal could ever be recorded "+
			"and the approver is back to silence", err)
	}
}

// TestARefusalDoesNotDecideTheProposalAtTheEngine. The Go stores keep approver
// and decided_at untouched; this is the same claim asked of the row an auditor
// reads, because the row is where "two people signed this order" is recorded and
// the whole of #558 is additive to it.
func TestARefusalDoesNotDecideTheProposalAtTheEngine(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	store := NewPostgres(pool).Proposals()
	if err := store.Put(ctx, heldOrder(t, "o-untouched", "user:alice@kanz", t0), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := store.RecordRefusal(ctx, "o-untouched", "user:alice@kanz", RefusalSelfApproval, t0.Add(time.Hour)); err != nil {
		t.Fatalf("RecordRefusal: %v", err)
	}
	var approver string
	var decidedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT approver, decided_at FROM order_proposals WHERE order_id = 'o-untouched'`,
	).Scan(&approver, &decidedAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if approver != "" || decidedAt != nil {
		t.Fatalf("the refused proposal is DECIDED in the database (approver=%q decided_at=%v) — "+
			"one person can destroy a colleague's pending decision by attempting their own",
			approver, decidedAt)
	}
}

// TestConcurrentClaimElectsExactlyOneWinner IS THE PROPERTY THE IN-MEMORY STORE
// CANNOT DEMONSTRATE (#498's recorded lesson).
//
// Two approvers acting on one pending order at the same moment both pass every
// check in Go — they are different people, the digest matches for both, neither
// has expired. If both are told they won, both admit the order, and one decision
// reaches a live venue twice. A sync.Mutex answers this within one process; the
// shipped OMS runs replicas: 2, so the arbitration has to be the engine's.
func TestConcurrentClaimElectsExactlyOneWinner(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	store := st.Proposals()
	ctx := context.Background()

	if err := store.Put(ctx, heldOrder(t, "o-race", "user:alice@kanz", t0), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// EVERY RACER CARRIES THE SAME ANNOUNCEMENT, so the outbox row count below is
	// a statement about how many claims COMMITTED and not about how many distinct
	// records were offered.
	raceAnnounce := approvalAnnounce(t, "o-race", t0.Add(time.Minute))

	const racers = 32
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		won    int
		lost   int
		winner string
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		approver := "user:approver-" + string(rune('a'+i%26)) + string(rune('0'+i/26)) + "@kanz"
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // released together, so they collide inside the engine
			ok, err := store.Claim(ctx, "o-race", approver, t0.Add(time.Minute), raceAnnounce)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("Claim(%s): %v", approver, err)
			case ok:
				won++
				winner = approver
			default:
				lost++
			}
		}()
	}
	close(start)
	wg.Wait()

	if won != 1 {
		t.Fatalf("%d approvers were told they won one pending order, want exactly 1 — more than "+
			"one means the same decision is admitted and routed more than once", won)
	}
	if lost != racers-1 {
		t.Fatalf("lost = %d, want %d", lost, racers-1)
	}

	// THE ROW MUST NAME THE CALLER THAT WON, not merely some approver. A store
	// that elected one winner and recorded a different name would produce an
	// audit trail nobody could reconcile against the order that traded.
	got, ok, err := store.Get(ctx, "o-race")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Approver != winner {
		t.Fatalf("the row records approver %q but %q was told it won", got.Approver, winner)
	}

	// THE LOSERS ANNOUNCED NOTHING (#410, clause (d)). Electing one winner is only
	// half the property now that the claim carries the ORDER_APPROVED FACT: if a
	// loser's rollback did not take its record with it, the audit trail would show
	// one order countersigned by 32 different people, which is a worse answer than
	// the silence this FACT replaced.
	queued, err := st.Outbox().Pending(ctx, "o-race", racers)
	if err != nil {
		t.Fatalf("outbox Pending: %v", err)
	}
	if len(queued) != 1 {
		t.Fatalf("%d approvers queued %d ORDER_APPROVED FACTs for one decision, want 1",
			racers, len(queued))
	}
}

// TestAnUnscopedSessionCannotReadProposals — MT-01e. An unscoped read must be an
// ERROR, never an empty answer: zero rows and "nobody said who is asking" are
// the same observable event otherwise, and the dangerous one is invisible.
func TestAnUnscopedSessionCannotReadProposals(t *testing.T) {
	pool := newPool(t)
	if err := NewPostgres(pool).Proposals().Put(context.Background(),
		heldOrder(t, "o-scope", "user:alice@kanz", t0), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}

	unscoped := poolAs(t, "")
	_, _, err := NewPostgres(unscoped).Proposals().Get(context.Background(), "o-scope")
	if err == nil {
		t.Fatal("a session with no app.tenant_id READ the proposals table — an unscoped query " +
			"returned an answer instead of failing, which is the silent-empty defect MT-01e exists " +
			"to end (here it would be worse than empty: whichever rows RLS did not filter are " +
			"orders another tenant is waiting to have approved)")
	}
	if !strings.Contains(err.Error(), "tenant scope missing") {
		t.Errorf("error = %v, want app_current_tenant()'s named refusal", err)
	}
}

// TestAnotherTenantCannotSeeOrApproveAProposal. RLS is load-bearing here in a way
// it is not on most tables: a proposal readable across tenants is a proposal
// APPROVABLE across tenants, and approving one admits an order that spends
// somebody else's capital.
func TestAnotherTenantCannotSeeOrApproveAProposal(t *testing.T) {
	pool := newPool(t) // tenant __system__
	ctx := context.Background()
	if err := NewPostgres(pool).Proposals().Put(ctx, heldOrder(t, "o-tenant", "user:alice@kanz", t0), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}

	other := NewPostgres(poolAs(t, "someone-else")).Proposals()
	if _, ok, err := other.Get(ctx, "o-tenant"); err != nil || ok {
		t.Fatalf("another tenant read the proposal (ok=%v err=%v)", ok, err)
	}
	pending, err := other.Pending(ctx, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("another tenant's pending queue shows %d of this tenant's held orders", len(pending))
	}
	won, err := other.Claim(ctx, "o-tenant", "user:bob@kanz", t0.Add(time.Minute), approvalAnnounce(t, "o-tenant", t0.Add(time.Minute)))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if won {
		t.Fatal("another tenant APPROVED this tenant's held order — the approval would admit an " +
			"order spending capital the approver has no relationship with")
	}
	// And it is still pending for the tenant that owns it.
	if got, ok, _ := NewPostgres(pool).Proposals().Get(ctx, "o-tenant"); !ok || got.Approver != "" {
		t.Fatal("the cross-tenant claim decided the proposal after all")
	}
}

// TestPutCommitsTheProposalAndItsFactTogether (#292's guarantee, applied to the
// hold). Either the order is held and the estate will hear about it, or neither
// is true — there is no instant at which a held order exists with no
// announcement behind it.
func TestPutCommitsTheProposalAndItsFactTogether(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	rec := outbox.Record{
		Subject: EventTypePendingApproval, EventType: EventTypePendingApproval,
		Domain: Domain, PartitionKey: "o-tx", TenantID: "acme", EventTime: t0,
		PayloadSchemaRef: "order.v1.OrderPendingApproval:1", Payload: []byte{},
	}
	if err := st.Proposals().Put(ctx, heldOrder(t, "o-tx", "user:alice@kanz", t0), []outbox.Record{rec}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	pending, err := st.Outbox().Pending(ctx, "o-tx", 10)
	if err != nil {
		t.Fatalf("outbox Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("the hold committed %d FACTs, want 1", len(pending))
	}

	// A REFUSED DUPLICATE ROLLS ITS FACT BACK WITH IT. Without that, every
	// redelivery of the command announces the same hold again.
	err = st.Proposals().Put(ctx, heldOrder(t, "o-tx", "user:alice@kanz", t0), []outbox.Record{rec})
	if !errors.Is(err, ErrProposalExists) {
		t.Fatalf("duplicate Put: %v, want ErrProposalExists", err)
	}
	pending, err = st.Outbox().Pending(ctx, "o-tx", 10)
	if err != nil {
		t.Fatalf("outbox Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("a refused duplicate left %d FACTs queued, want 1", len(pending))
	}
}

// TestAProposalWithNoTenantOnTheRecordHoldsNothing. outbox.From refuses a record
// with no tenant, but a caller could still hand Put one. The enqueue is INSIDE
// the transaction, so its failure must roll the hold back — an order whose hold
// cannot be announced must not be held silently.
func TestAProposalWithNoTenantOnTheRecordHoldsNothing(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	bad := outbox.Record{
		Subject: EventTypePendingApproval, EventType: EventTypePendingApproval,
		Domain: Domain, PartitionKey: "o-notenant", EventTime: t0,
		PayloadSchemaRef: "order.v1.OrderPendingApproval:1", Payload: []byte{},
		// TenantID deliberately empty.
	}
	if err := st.Proposals().Put(ctx, heldOrder(t, "o-notenant", "user:alice@kanz", t0), []outbox.Record{bad}); err == nil {
		t.Fatal("Put accepted a FACT with no tenant — the record could never be published, so the " +
			"order would be held durably and announced to nobody")
	}
	if _, ok, err := st.Proposals().Get(ctx, "o-notenant"); err != nil || ok {
		t.Fatalf("the hold survived a failed enqueue (ok=%v err=%v) — the two are supposed to be "+
			"one transaction", ok, err)
	}
}

// expiryFact is a well-formed announcement that a held order died of the clock.
// Hand-built rather than taken from Emitter.RejectedFact because that one needs a
// ctx carrying an inbound delivery for its tenant, and a sweep has none — the
// same reason TestPutCommitsTheProposalAndItsFactTogether builds its own.
func expiryFact(orderID string) outbox.Record {
	return outbox.Record{
		Subject: EventTypeRejected, EventType: EventTypeRejected,
		Domain: Domain, PartitionKey: orderID, TenantID: "acme", EventTime: t0,
		PayloadSchemaRef: "order.v1.OrderRejected:1", Payload: []byte{},
	}
}

// announcedAt reads the marker with RAW SQL rather than through the store.
//
// The store's own reader is part of what is under test here: a Get that dropped
// the column would make "never announced" and "announced" the same answer, and
// every assertion below would still pass. The column is read from the engine
// instead.
func announcedAt(t *testing.T, pool *pgxpool.Pool, orderID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT expiry_announced_at FROM order_proposals WHERE order_id = $1`, orderID).Scan(&at); err != nil {
		t.Fatalf("read expiry_announced_at for %s: %v", orderID, err)
	}
	return at
}

// TestConcurrentExpiryAnnouncesExactlyOnce IS THE OTHER PROPERTY NO IN-MEMORY
// STORE CAN DEMONSTRATE, and the reason the marker is a column rather than a
// field on a sweeper (#539).
//
// Expiry is not an act anybody performs: it is a deadline passing, so EVERY
// replica's sweeper sees the same expired row on the same tick with nothing to
// serialise them but the row itself. If two are told they won, one dead order
// produces two ORDER_REJECTED FACTs — and a duplicate rejection is not a cosmetic
// double: the ledger, the position projector and the trader's client each fold a
// second terminal transition for an order that only died once. A mutex answers
// this within one process; the shipped OMS runs replicas: 2.
func TestConcurrentExpiryAnnouncesExactlyOnce(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	store := st.Proposals()
	ctx := context.Background()

	held := heldOrder(t, "o-expiry-race", "user:alice@kanz", t0)
	if err := store.Put(ctx, held, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// PAST THE DEADLINE, taken from the proposal rather than from a constant, so
	// this test does not silently stop testing expiry if the TTL changes.
	after := held.ExpiresAt.Add(time.Minute)

	const racers = 32
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		won  int
		lost int
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // released together, so they collide inside the engine
			ok, err := store.AnnounceExpiry(ctx, "o-expiry-race", after,
				[]outbox.Record{expiryFact("o-expiry-race")})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("AnnounceExpiry: %v", err)
			case ok:
				won++
			default:
				lost++
			}
		}()
	}
	close(start)
	wg.Wait()

	if won != 1 {
		t.Fatalf("%d sweepers were told they announced one expiry, want exactly 1 — every extra "+
			"winner is a second ORDER_REJECTED for an order that died once, folded as a second "+
			"terminal transition by everything downstream", won)
	}
	if lost != racers-1 {
		t.Fatalf("lost = %d, want %d", lost, racers-1)
	}

	// THE LOSERS ENQUEUED NOTHING. Winning is only half the property: if a loser's
	// rollback did not take its FACT with it, the estate still hears the rejection
	// 32 times and the exactly-once verdict above is decoration.
	pending, err := st.Outbox().Pending(ctx, "o-expiry-race", racers)
	if err != nil {
		t.Fatalf("outbox Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d sweepers queued %d FACTs for one expiry, want 1", racers, len(pending))
	}

	// AND THE ROW IS MARKED, so the next tick — on either pod, after any restart —
	// does not start the race again. A won race with no durable marker is a
	// re-announcement every sweep interval, forever.
	if announcedAt(t, pool, "o-expiry-race") == nil {
		t.Fatal("a sweeper was told it won but expiry_announced_at is still NULL — the next tick " +
			"announces the same expiry again")
	}

	// AN EXPIRY IS NOT AN APPROVAL. The row must stay unapproved: it is the
	// evidence that nobody signed this order, and an approver written by the sweep
	// would read to an auditor as a second signature that never happened.
	got, ok, err := store.Get(ctx, "o-expiry-race")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Approver != "" || !got.DecidedAt.IsZero() {
		t.Fatalf("the sweep decided the proposal (approver=%q decided_at=%v) — an expiry is the "+
			"absence of a decision, not one", got.Approver, got.DecidedAt)
	}
}

// TestTheDatabaseRefusesAnExpiryAnnouncedBeforeItHappened.
//
// AnnounceExpiry re-checks the deadline in its own predicate, and the sweeper
// only looks at rows already past it. THIS TEST GOES ROUND BOTH, with raw SQL,
// because the constraint exists for the writer that does not come through them —
// and because the ordinary way this happens is not a rogue writer but a pod whose
// clock is ahead. An expiry announced early rejects an order somebody still had
// time to sign, and the trader's notice and the audit row would then AGREE on a
// deadline that had not passed, which is worse than either being wrong alone.
func TestTheDatabaseRefusesAnExpiryAnnouncedBeforeItHappened(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	held := heldOrder(t, "o-expiry-early", "user:alice@kanz", t0)
	if err := NewPostgres(pool).Proposals().Put(ctx, held, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, at := range []time.Time{
		held.CreatedAt,                        // the moment it was held
		held.ExpiresAt.Add(-time.Second),      // one second of clock skew
		held.ExpiresAt.Add(-time.Microsecond), // the narrowest early Postgres can store
	} {
		_, err := pool.Exec(ctx,
			`UPDATE order_proposals SET expiry_announced_at = $2 WHERE order_id = $1`,
			"o-expiry-early", at)
		if err == nil {
			t.Fatalf("the database ACCEPTED an expiry announced at %v on a proposal that expires at "+
				"%v — the row records the rejection of an order that had not run out of time",
				at, held.ExpiresAt)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("announced-at %v was refused by something other than a CHECK constraint (%v) — "+
				"the refusal must come from the engine, or it is not there for a writer that "+
				"bypasses Go", at, err)
		}
		if !strings.Contains(pgErr.ConstraintName, "announced_after_expiry") {
			t.Errorf("refused by constraint %q, want the announced-after-expiry one — a different "+
				"constraint passing for this is how the real one goes missing unnoticed",
				pgErr.ConstraintName)
		}
	}

	// AND THE DEADLINE ITSELF IS ACCEPTED, so the test above cannot pass against a
	// constraint that refuses every announcement. `>=`, not `>`: a sweeper running
	// at the exact instant of expiry is on time, not early.
	if _, err := pool.Exec(ctx,
		`UPDATE order_proposals SET expiry_announced_at = $2 WHERE order_id = $1`,
		"o-expiry-early", held.ExpiresAt); err != nil {
		t.Fatalf("the CHECK refused an announcement made AT the deadline (%v) — an expiry could "+
			"never be recorded and the sweep would fail on every tick", err)
	}
}

// TestAnnounceExpiryCommitsTheMarkerAndItsFactTogether (#292's guarantee, applied
// to the sweep). Either the expiry is marked announced and the estate will hear
// about it, or neither is true.
//
// THE MARKER IS THE DANGEROUS HALF. It is a promise that the FACT was published,
// and every later tick reads it: set without its FACT, the order is expired,
// unannounced and PERMANENTLY invisible to the sweeper that exists to announce
// it — the exact silence of #539, now with a row asserting it had been broken.
func TestAnnounceExpiryCommitsTheMarkerAndItsFactTogether(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	store := st.Proposals()
	ctx := context.Background()

	held := heldOrder(t, "o-expiry-tx", "user:alice@kanz", t0)
	if err := store.Put(ctx, held, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	after := held.ExpiresAt.Add(time.Minute)

	// A RECORD WITH NO TENANT ROLLS THE WHOLE THING BACK. outbox.From refuses one,
	// but a caller can still hand AnnounceExpiry a hand-built record; the enqueue
	// is INSIDE the transaction, so its failure must take the marker with it.
	bad := expiryFact("o-expiry-tx")
	bad.TenantID = "" // could never be published: it would sit at the head of its key
	if _, err := store.AnnounceExpiry(ctx, "o-expiry-tx", after, []outbox.Record{bad}); err == nil {
		t.Fatal("AnnounceExpiry accepted a FACT with no tenant — the record could never be " +
			"published, so the expiry would be marked announced and announced to nobody")
	}
	if at := announcedAt(t, pool, "o-expiry-tx"); at != nil {
		t.Fatalf("the marker survived a failed enqueue (announced at %v) — the two are supposed to "+
			"be one transaction, and the order is now expired, unannounced and invisible to every "+
			"later sweep", *at)
	}
	if p, err := st.Outbox().Pending(ctx, "o-expiry-tx", 10); err != nil || len(p) != 0 {
		t.Fatalf("a refused announcement left %d FACTs queued (err=%v), want 0", len(p), err)
	}

	// THE HAPPY PATH: marker and FACT together.
	ok, err := store.AnnounceExpiry(ctx, "o-expiry-tx", after, []outbox.Record{expiryFact("o-expiry-tx")})
	if err != nil || !ok {
		t.Fatalf("AnnounceExpiry: ok=%v err=%v", ok, err)
	}
	pending, err := st.Outbox().Pending(ctx, "o-expiry-tx", 10)
	if err != nil {
		t.Fatalf("outbox Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("the announcement committed %d FACTs, want 1", len(pending))
	}
	if pending[0].Record.EventType != EventTypeRejected {
		t.Errorf("queued %q, want %q — the trader is owed the terminal FACT, not any FACT",
			pending[0].Record.EventType, EventTypeRejected)
	}
	at := announcedAt(t, pool, "o-expiry-tx")
	if at == nil {
		t.Fatal("the FACT is queued but expiry_announced_at is NULL — the next sweep announces it again")
	}

	// A SECOND CALL ANNOUNCES NOTHING, and that is exactly-once across TICKS rather
	// than across pods: one sweeper, restarted or simply running again a minute
	// later, must not re-announce what it already announced.
	ok, err = store.AnnounceExpiry(ctx, "o-expiry-tx", after.Add(time.Hour),
		[]outbox.Record{expiryFact("o-expiry-tx")})
	if err != nil {
		t.Fatalf("second AnnounceExpiry: %v", err)
	}
	if ok {
		t.Fatal("the second sweep was told it won an expiry already announced — every tick would " +
			"publish another ORDER_REJECTED for the same dead order")
	}
	if p, _ := st.Outbox().Pending(ctx, "o-expiry-tx", 10); len(p) != 1 {
		t.Fatalf("a refused second announcement left %d FACTs queued, want 1", len(p))
	}
	if got := announcedAt(t, pool, "o-expiry-tx"); got == nil || !got.Equal(*at) {
		t.Errorf("the refused call moved the marker (%v -> %v) — the row must keep the moment the "+
			"expiry was ACTUALLY announced, or a late sweeper is indistinguishable from a prompt one",
			*at, got)
	}
}

// TestAnotherTenantCannotSeeOrAnnounceAnExpiredProposal. RLS on the sweep path is
// load-bearing in its own way: the sweeper is a background loop with no request
// behind it, so nothing but the row's tenant scopes what it may kill. A proposal
// visible across tenants is a proposal another tenant's sweeper can declare dead,
// and the FACT it publishes lands under ITS tenant — so the owner is told nothing
// while somebody else's estate is told about an order that is not theirs.
func TestAnotherTenantCannotSeeOrAnnounceAnExpiredProposal(t *testing.T) {
	pool := newPool(t) // tenant __system__
	ctx := context.Background()
	held := heldOrder(t, "o-expiry-tenant", "user:alice@kanz", t0)
	if err := NewPostgres(pool).Proposals().Put(ctx, held, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	after := held.ExpiresAt.Add(time.Minute)

	other := NewPostgres(poolAs(t, "someone-else")).Proposals()
	expired, err := other.ExpiredUnannounced(ctx, after, 10)
	if err != nil {
		t.Fatalf("ExpiredUnannounced: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("another tenant's sweep queue shows %d of this tenant's expired orders", len(expired))
	}
	won, err := other.AnnounceExpiry(ctx, "o-expiry-tenant", after,
		[]outbox.Record{expiryFact("o-expiry-tenant")})
	if err != nil {
		t.Fatalf("AnnounceExpiry: %v", err)
	}
	if won {
		t.Fatal("another tenant ANNOUNCED the expiry of this tenant's held order — the rejection " +
			"would be published under the wrong tenant and the owner would still be waiting")
	}
	if at := announcedAt(t, pool, "o-expiry-tenant"); at != nil {
		t.Fatalf("the cross-tenant sweep marked the row after all (announced at %v)", *at)
	}

	// AND IT IS STILL WORK THE OWNER'S SWEEP WILL DO. A cross-tenant call that
	// silently consumed the row would leave the order expired, unannounced and
	// invisible — the defect, reintroduced through the tenant boundary.
	mine, err := NewPostgres(pool).Proposals().ExpiredUnannounced(ctx, after, 10)
	if err != nil {
		t.Fatalf("owner ExpiredUnannounced: %v", err)
	}
	if len(mine) != 1 || mine[0].ID != "o-expiry-tenant" {
		t.Fatalf("the owner's sweep queue holds %d rows, want the one expired proposal", len(mine))
	}
}

// THE PENDING INDEX WAS UNBOUNDED (#548). Everything below is about one index.
//
// 0009 justified `WHERE approver = ''` with "a decided proposal leaves the index,
// so listing what needs attention stays the same cost on day 1000 as on day 1".
// That is true of an APPROVED proposal and false of an EXPIRED one: nobody signs
// an order nobody reached in time, so approver stays '' for the life of the row
// and the row never leaves. Every proposal that ever expired unsigned was still
// an entry, and the pending list — the screen an approver is looking at — got
// slower with the AGE of the deployment rather than with the length of its queue.
// Nothing failed while that happened, which is why it needs a test rather than an
// incident.
//
// 0011 drops order_proposals_pending_idx and renames 0010's narrower
// order_proposals_expiry_sweep_idx to order_proposals_open_idx, so one index
// serves the pending read and the expiry sweep; Pending spells out
// `expiry_announced_at IS NULL` so the planner can prove the predicate. The three
// ways that can go wrong are each below: the clause could be redundant rather
// than load-bearing (the plan), the old index could survive the rename (the
// catalog), and the marker could fail to actually retire a row (the count).

// pendingQuery is the statement PostgresProposals.Pending issues, repeated here
// because EXPLAIN must run on the exact text the store runs — a plan for a query
// nobody executes proves nothing about the one that does.
//
// THE COPY IS RECONCILED, NOT TRUSTED. TestThePendingReadIsServedByTheOpenIndex
// runs both this text and Pending against the same rows and fails if they return
// different proposals, so a change to the store's WHERE that is not made here
// surfaces as a row-set mismatch instead of as a green EXPLAIN of a query the OMS
// stopped issuing.
const pendingQuery = `
	SELECT order_id, portfolio_id, act, proposer, digest, command, approver, decided_at, created_at, expires_at,
	       refusal_reason, refused_by, refused_at, expiry_announced_at
	FROM order_proposals
	WHERE approver = '' AND expiry_announced_at IS NULL AND expires_at > $1
	ORDER BY created_at, order_id`

// explainPlan returns the planner's chosen plan for sql as one string.
//
// Plain EXPLAIN, not EXPLAIN ANALYZE: the question is which plan Postgres CHOOSES
// for this predicate, and executing it would answer a different one — a sequential
// scan of a test table is fast, and "fast" is exactly the reading that lets an
// index nobody can use look healthy.
func explainPlan(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "EXPLAIN "+sql, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan line: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	return plan.String()
}

// seedProposalBacklog fills the table with announced expired proposals and live
// pending ones, then ANALYZEs so the planner chooses on real statistics.
//
// THE SIZE IS THE POINT. On a ten-row table Postgres reads sequentially whatever
// the indexes say, so a plan test against a handful of rows asserts nothing; the
// backlog is what makes the index a choice the planner has a reason to make. The
// rows are written with raw SQL rather than through Put because they stand in for
// history — twenty thousand orders held over a deployment's life — and the store
// is not what is under test here.
func seedProposalBacklog(t *testing.T, pool *pgxpool.Pool, announced, live int, now time.Time) {
	t.Helper()
	ctx := context.Background()

	// Expired, unsigned and ANNOUNCED: the rows 0009's index kept forever.
	// expiry_announced_at = expires_at satisfies 0010's announced-after-expiry
	// CHECK, so this backlog is one the engine would actually have accepted.
	if _, err := pool.Exec(ctx, `
		INSERT INTO order_proposals
			(order_id, portfolio_id, act, proposer, digest, command,
			 created_at, expires_at, expiry_announced_at)
		SELECT 'o-backlog-' || g, 'pf-1', 'submit_order', 'user:alice@kanz', 'sha256:seed', ''::bytea,
		       $1::timestamptz - (g || ' seconds')::interval - interval '1 hour',
		       $1::timestamptz - (g || ' seconds')::interval,
		       $1::timestamptz - (g || ' seconds')::interval
		FROM generate_series(1, $2::int) AS g
	`, now, announced); err != nil {
		t.Fatalf("seed announced backlog: %v", err)
	}

	// Still awaiting a signature: the only rows the pending list is asked for.
	if _, err := pool.Exec(ctx, `
		INSERT INTO order_proposals
			(order_id, portfolio_id, act, proposer, digest, command, created_at, expires_at)
		SELECT 'o-live-' || g, 'pf-1', 'submit_order', 'user:alice@kanz', 'sha256:seed', ''::bytea,
		       $1::timestamptz - interval '1 minute' + (g || ' milliseconds')::interval,
		       $1::timestamptz + interval '1 hour'
		FROM generate_series(1, $2::int) AS g
	`, now, live); err != nil {
		t.Fatalf("seed live proposals: %v", err)
	}

	// WITHOUT THIS THE PLAN IS GUESSWORK. An unanalyzed table gives the planner
	// default estimates, and a test that passed on those would not be reading the
	// choice a production table's statistics produce.
	if _, err := pool.Exec(ctx, `ANALYZE order_proposals`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
}

// openQueueDepth counts the entries order_proposals_open_idx holds, by counting
// what its predicate matches rather than by measuring the index.
//
// pg_relation_size answers in PAGES, and a btree does not hand a page back when
// its entries are retired — a bounded index and an unbounded one report the same
// bytes until a VACUUM no test can rely on. The predicate is the property; the
// file size is a lagging indicator of it.
func openQueueDepth(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM order_proposals WHERE approver = '' AND expiry_announced_at IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count open proposals: %v", err)
	}
	return n
}

// proposalRows is every row on the table, announced or not — the half of the
// boundedness claim that must NOT change. An index shrinking because the rows
// were deleted would be a far worse fix: those rows are the OMS's only durable
// evidence that an order was held and how it ended.
func proposalRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM order_proposals`).Scan(&n); err != nil {
		t.Fatalf("count proposals: %v", err)
	}
	return n
}

// TestThePendingReadIsServedByTheOpenIndex IS THE ASSERTION THAT THE ADDED CLAUSE
// DOES SOMETHING.
//
// Postgres uses a partial index only where it can PROVE the predicate holds, so
// `expiry_announced_at IS NULL` in Pending's WHERE is not decoration beside the
// empty-approver clause — it is the only thing that lets the planner reach
// order_proposals_open_idx at all. Spelled without it this read is a sequential
// scan of every order the deployment ever held: no error, no warning, just an
// approver's queue that takes longer every week. That is the trap 0009 documented
// and then fell into, and reading the PLAN is the only way to tell a clause that
// enabled an index from one that was already implied.
func TestThePendingReadIsServedByTheOpenIndex(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	now := t0.Add(24 * time.Hour)

	// 20,000 proposals that expired unsigned and were announced, against 10 still
	// awaiting a signature. THAT RATIO IS THE DEFECT: under 0009's predicate every
	// one of the 20,000 was still an index entry, because nobody signs an order
	// nobody reached in time and approver never stops being ''.
	seedProposalBacklog(t, pool, 20000, 10, now)

	plan := explainPlan(t, pool, pendingQuery, now)
	if !strings.Contains(plan, "order_proposals_open_idx") {
		t.Fatalf("the pending read does not reach order_proposals_open_idx:\n%s\n"+
			"a partial index whose predicate the planner cannot prove is an index that does not "+
			"exist, and this read then walks every proposal ever held", plan)
	}
	if strings.Contains(plan, "Seq Scan") {
		t.Errorf("the pending read still contains a sequential scan:\n%s\n"+
			"the queue an approver is looking at must cost what the queue is worth, not what the "+
			"table's history is worth", plan)
	}

	// AND THE CLAUSE IS WHAT DID IT — the before/after this issue exists for, in
	// one assertion. Spelled 0009's way the same read cannot reach the index; if it
	// ever can, the index stopped being partial on the marker and the boundedness
	// proved below went with it.
	old := strings.Replace(pendingQuery, "AND expiry_announced_at IS NULL ", "", 1)
	if old == pendingQuery {
		t.Fatal("the control query is identical to the real one — the clause this test claims is " +
			"load-bearing is not in pendingQuery, so nothing here is a comparison")
	}
	if oldPlan := explainPlan(t, pool, old, now); strings.Contains(oldPlan, "order_proposals_open_idx") {
		t.Errorf("the read WITHOUT `expiry_announced_at IS NULL` also used order_proposals_open_idx:"+
			"\n%s\nthe clause proves nothing then, and neither does the assertion above", oldPlan)
	}

	// THE EXPLAINED TEXT IS THE TEXT Pending RUNS. An EXPLAIN of a query the store
	// no longer issues is a green plan for a plan nobody uses, so both are read
	// against the same rows and must return the same proposals.
	rows, err := pool.Query(ctx, pendingQuery, now)
	if err != nil {
		t.Fatalf("run the explained query: %v", err)
	}
	var explained []string
	for rows.Next() {
		prop, err := scanOrderProposal(rows)
		if err != nil {
			rows.Close()
			t.Fatalf("scan explained row: %v", err)
		}
		explained = append(explained, prop.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("run the explained query: %v", err)
	}
	got, err := NewPostgres(pool).Proposals().Pending(ctx, now)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	var listed []string
	for _, p := range got {
		listed = append(listed, p.ID)
	}
	if strings.Join(explained, ",") != strings.Join(listed, ",") {
		t.Fatalf("the explained query returned %v and Pending returned %v — pendingQuery has drifted "+
			"from the store, so the plan asserted above belongs to a statement the OMS does not issue",
			explained, listed)
	}
	if len(listed) != 10 {
		t.Fatalf("Pending listed %d of 10 live proposals against a 20,000-row backlog", len(listed))
	}
}

// TestTheUnboundedPendingIndexIsGone. The drop is half of 0011: an index left
// behind is still maintained on every hold, still grows with every expiry nobody
// signs, and still makes this table's write cost climb with the age of the
// deployment — all while the plan test above passes, because the read would use
// the new index either way. The catalog is the only place that difference shows.
//
// READING pg_indexes IN A TEST IS NOT WHAT test/arch/migration_table_discovery_test.go
// BANS. That guard scans services/*/migrations/*.sql for pg_class, relrowsecurity
// and information_schema.tables, and it bans a MIGRATION discovering the tables it
// rewrites — the defect where whichever service's DO block ran last silently
// rewrote another service's policies. A test asserting the outcome of a migration
// that named its own index is the opposite of that: it is how the migration is
// checked.
func TestTheUnboundedPendingIndexIsGone(t *testing.T) {
	pool := newPool(t)
	rows, err := pool.Query(context.Background(),
		`SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = $1 AND tablename = 'order_proposals'`,
		testSchema)
	if err != nil {
		t.Fatalf("read pg_indexes: %v", err)
	}
	defer rows.Close()
	defs := map[string]string{}
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		defs[name] = def
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read pg_indexes: %v", err)
	}

	for _, gone := range []string{
		"order_proposals_pending_idx",      // 0009's, the unbounded one
		"order_proposals_expiry_sweep_idx", // 0010's, renamed rather than duplicated
	} {
		if def, ok := defs[gone]; ok {
			t.Errorf("%s survives 0011 (%s) — two indexes over one predicate means every hold pays to "+
				"maintain both, and the one this issue exists to retire keeps growing unread", gone, def)
		}
	}

	def, ok := defs["order_proposals_open_idx"]
	if !ok {
		t.Fatalf("order_proposals_open_idx does not exist; the table has %v — the pending read and the "+
			"expiry sweep would both fall back to a sequential scan", indexNames(defs))
	}
	// AND IT IS THE NARROW ONE. An index under the new name carrying only
	// `approver = ''` would be 0009's index renamed: every assertion above would
	// still pass and the growth would be untouched.
	if !strings.Contains(def, "expiry_announced_at IS NULL") || !strings.Contains(def, "approver") {
		t.Errorf("order_proposals_open_idx is %s, which does not carry both predicates — without "+
			"`expiry_announced_at IS NULL` it is the unbounded index under a new name", def)
	}
}

// indexNames says what the table DOES carry, so a failure above names the index
// that exists rather than only the one that does not.
func indexNames(defs map[string]string) []string {
	out := make([]string, 0, len(defs))
	for name := range defs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestAnAnnouncedExpiryLeavesTheIndexAndKeepsItsRow IS THE BOUNDEDNESS PROPERTY,
// and it is the one the plan test cannot show: a query can use a perfectly narrow
// index that nothing ever leaves.
//
// Under 0009 an expired proposal was in the pending index forever, because expiry
// is not a decision and approver never stops being empty. What retires a row now is
// the marker 0010 added — the sweep that announces the expiry is also what takes
// the row out of the index — so the index is bounded by the TTL instead of by the
// age of the deployment. The two counts below are the whole claim: the index
// empties, the TABLE does not.
func TestAnAnnouncedExpiryLeavesTheIndexAndKeepsItsRow(t *testing.T) {
	pool := newPool(t)
	store := NewPostgres(pool).Proposals()
	ctx := context.Background()

	const held = 24
	var last time.Time
	for i := 0; i < held; i++ {
		p := heldOrder(t, fmt.Sprintf("o-bound-%02d", i), "user:alice@kanz", t0.Add(time.Duration(i)*time.Second))
		if err := store.Put(ctx, p, nil); err != nil {
			t.Fatalf("Put %s: %v", p.ID, err)
		}
		last = p.ExpiresAt
	}
	// Past every deadline, taken from the proposals rather than from a constant, so
	// this test does not quietly stop testing expiry if the TTL changes.
	after := last.Add(time.Minute)

	if got := openQueueDepth(t, pool); got != held {
		t.Fatalf("the index holds %d entries for %d held orders, want %d", got, held, held)
	}

	// NOBODY SIGNS ANY OF THEM — the case 0009's justification did not cover.
	for i := 0; i < held; i++ {
		id := fmt.Sprintf("o-bound-%02d", i)
		ok, err := store.AnnounceExpiry(ctx, id, after, []outbox.Record{expiryFact(id)})
		if err != nil || !ok {
			t.Fatalf("AnnounceExpiry(%s): ok=%v err=%v", id, ok, err)
		}
	}

	if got := openQueueDepth(t, pool); got != 0 {
		t.Fatalf("%d of %d announced expiries are STILL in the index — an entry nothing retires is an "+
			"index that grows with the age of the deployment, and the pending list then gets slower "+
			"every week with nothing failing", got, held)
	}
	// AND THE HISTORY IS INTACT. An index emptied by deleting its rows would be a
	// far worse fix: these rows are the OMS's only durable record that the order was
	// held, by whom, and that it died of the clock rather than of a decision.
	if got := proposalRows(t, pool); got != held {
		t.Fatalf("the table holds %d rows, want %d — the index was bounded by throwing the audit "+
			"trail away", got, held)
	}
	// AND EACH ROW IS STILL UNAPPROVED, so they left the index because they were
	// ANNOUNCED and not because something wrote a signature nobody gave.
	got, ok, err := store.Get(ctx, "o-bound-00")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Approver != "" {
		t.Fatalf("the sweep recorded approver %q — the row left the index by claiming a second "+
			"signature that never happened", got.Approver)
	}
}

// TestPendingStillListsExactlyWhatNeedsASignature is the behavioural half of
// #548: a clause added for the planner must not change WHICH proposals an
// approver is shown.
//
// An optimisation that quietly narrowed this list would be the worse defect by
// far — a held order missing from the pending list is invisible, and invisible is
// exactly what #539 already cost once on this table. The four rows here are the
// four states the predicate has to sort out, and the approved one is deliberately
// still LIVE so that its exclusion is the signature and not the clock.
func TestPendingStillListsExactlyWhatNeedsASignature(t *testing.T) {
	pool := newPool(t)
	store := NewPostgres(pool).Proposals()
	ctx := context.Background()

	dead := heldOrder(t, "o-open-expired", "user:alice@kanz", t0)
	if err := store.Put(ctx, dead, nil); err != nil {
		t.Fatalf("Put expired: %v", err)
	}
	// Every "now" below is taken from a proposal's own deadline, never a constant.
	now := dead.ExpiresAt.Add(time.Minute)

	announced := heldOrder(t, "o-open-announced", "user:alice@kanz", t0)
	if err := store.Put(ctx, announced, nil); err != nil {
		t.Fatalf("Put announced: %v", err)
	}
	if ok, err := store.AnnounceExpiry(ctx, "o-open-announced", now,
		[]outbox.Record{expiryFact("o-open-announced")}); err != nil || !ok {
		t.Fatalf("AnnounceExpiry: ok=%v err=%v", ok, err)
	}

	live := heldOrder(t, "o-open-live", "user:alice@kanz", now)
	if err := store.Put(ctx, live, nil); err != nil {
		t.Fatalf("Put live: %v", err)
	}
	signed := heldOrder(t, "o-open-signed", "user:alice@kanz", now)
	if err := store.Put(ctx, signed, nil); err != nil {
		t.Fatalf("Put signed: %v", err)
	}
	if ok, err := store.Claim(ctx, "o-open-signed", "user:bob@kanz", now, approvalAnnounce(t, "o-open-signed", now)); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}

	pending, err := store.Pending(ctx, now)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	var ids []string
	for _, p := range pending {
		ids = append(ids, p.ID)
	}
	if len(ids) != 1 || ids[0] != "o-open-live" {
		t.Fatalf("Pending listed %v, want exactly [o-open-live] — the live proposal is the only one "+
			"anybody can still act on, and it is the one an approver would never know about if the "+
			"index clause narrowed this list", ids)
	}

	// THE EXPIRED-UNANNOUNCED ONE IS NOT LOST, it is somebody else's queue. Pending
	// dropping it is only correct because the sweep picks it up; if both filtered it
	// out the order would be exactly as silent as it was before #539.
	expired, err := store.ExpiredUnannounced(ctx, now, 10)
	if err != nil {
		t.Fatalf("ExpiredUnannounced: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != "o-open-expired" {
		var got []string
		for _, p := range expired {
			got = append(got, p.ID)
		}
		t.Fatalf("the sweep queue holds %v, want exactly [o-open-expired] — a proposal in neither "+
			"queue is an order held durably that nobody will ever be told about", got)
	}
}
