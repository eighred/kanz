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
// Three properties here cannot be shown by the in-memory store at all:
//
//  1. the CHECK refuses a self-approval AT THE ENGINE, so a writer that does not
//     come through Claim still cannot forge the row an auditor reads;
//  2. RLS scopes the table, so a proposal is neither readable nor APPROVABLE
//     across tenants;
//  3. two concurrent Claims elect exactly one winner ACROSS CONNECTIONS — a
//     mutex in one process says nothing about the other pod, and the shipped OMS
//     runs replicas: 2.

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
