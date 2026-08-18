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
	"os"
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
	store := NewPostgres(pool).Proposals()
	ctx := context.Background()

	if err := store.Put(ctx, heldOrder(t, "o-race", "user:alice@kanz", t0), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}

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
			ok, err := store.Claim(ctx, "o-race", approver, t0.Add(time.Minute))
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
	won, err := other.Claim(ctx, "o-tenant", "user:bob@kanz", t0.Add(time.Minute))
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
