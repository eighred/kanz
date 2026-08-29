package store

// THE DURABLE HALF OF MAKER-CHECKER (#410), PROVEN WHERE IT CAN BE.
//
// #495 shipped PostgresProposals, migration 0004's actor <> approver CHECK, and
// an RLS policy, with NO Postgres-gated test — and said so. That is the weakest
// possible position for exactly these three things: a CHECK constraint, an RLS
// policy and an atomic DELETE are the parts that CANNOT be verified by reasoning
// about the Go code, because their behaviour lives in the database.
//
// These tests skip without TEST_POSTGRES_URL, like the fourteen others in this
// repository. Skipping is not passing: the value is that CI, which has a
// NOSUPERUSER role and a real Postgres, runs them.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	masterpb "github.com/eighred/kanz/kanz-schemas-go/master/v1"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
)

var pgNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func seedException(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if err := NewPostgresExceptions(pool, "__system__").Add(context.Background(), pricing.Exception{
		ID: id, Kind: pricing.KindPriceTolerance, InstrumentID: "INST1",
		Detail: "outlier", Status: pricing.StatusOpen, DetectedAt: pgNow,
	}); err != nil {
		t.Fatalf("seed exception: %v", err)
	}
}

func proposalFor(t *testing.T, id, exceptionID, proposer, price string) OverrideProposal {
	t.Helper()
	rat, ok := new(big.Rat).SetString(price)
	if !ok {
		t.Fatalf("bad price %q", price)
	}
	base, err := dualcontrol.Propose(id, dualcontrol.ActPricingOverride, exceptionID, proposer,
		PayloadDigest(exceptionID, "vendor confirmed", rat), pgNow, dualcontrol.DefaultTTL)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	return OverrideProposal{Proposal: base, Reason: "vendor confirmed", ChosenPrice: rat}
}

// THE DATABASE REFUSES A SELF-APPROVAL.
//
// The handler refuses it and pricing.Override.Validate refuses it, but this trail
// is the audit evidence an examiner reads. A row where actor = approver reads as
// four-eyes while one person held both signatures, so the constraint is the last
// line — the one a future caller that bypasses the handler still meets.
func TestPostgresOverride_TheDatabaseRefusesASelfApproval(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	es := NewPostgresExceptions(pool, "__system__")
	seedException(t, pool, "SELF:PRICE_TOLERANCE:ICE")

	// Go's Validate refuses these first, so the constraint is reached by writing
	// past it — which is precisely the caller this constraint exists for.
	for _, approver := range []string{"alice@kanz", "Alice@kanz", "  alice@kanz  "} {
		_, err := pool.Exec(ctx, `
			INSERT INTO exception_overrides (exception_id, actor, approver, reason, chosen_price, overridden_at)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, "SELF:PRICE_TOLERANCE:ICE", "alice@kanz", approver, "r", "130", pgNow)
		if err == nil {
			t.Errorf("Postgres accepted an override where the approver (%q) is the actor — a "+
				"self-approval is recorded as dual control in the audit trail", approver)
			continue
		}
		if !strings.Contains(err.Error(), "approver_is_not_actor") {
			t.Errorf("approver %q was refused, but not by the CHECK constraint: %v", approver, err)
		}
	}

	// AND A GENUINE SECOND SIGNATURE STILL WRITES. A constraint that refused
	// everything would pass every assertion above.
	rat, _ := new(big.Rat).SetString("130")
	if err := es.Override(ctx, "SELF:PRICE_TOLERANCE:ICE", pricing.Override{
		Actor: "alice@kanz", Approver: "bob@kanz", Reason: "vendor confirmed",
		ChosenPrice: rat, At: pgNow,
	}, Claim{}); err != nil {
		t.Fatalf("a genuine dual-signed override was refused: %v", err)
	}
}

// THE APPROVER SURVIVES THE ROUND TRIP, AND ” MEANS SINGLE-SIGNED.
//
// The column was added with DEFAULT ” so existing rows read as one-person
// overrides, which is true of them. If the read path dropped it, every dual
// signature would come back looking single — the write half's tests would still
// pass, and the audit trail would be silently wrong.
func TestPostgresOverride_TheApproverRoundTrips(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	es := NewPostgresExceptions(pool, "__system__")
	seedException(t, pool, "RT:PRICE_TOLERANCE:ICE")

	rat, _ := new(big.Rat).SetString("130")
	if err := es.Override(ctx, "RT:PRICE_TOLERANCE:ICE", pricing.Override{
		Actor: "alice@kanz", Approver: "bob@kanz", Reason: "dual", ChosenPrice: rat, At: pgNow,
	}, Claim{}); err != nil {
		t.Fatal(err)
	}
	ex, ok, err := es.Get(ctx, "RT:PRICE_TOLERANCE:ICE")
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if len(ex.Overrides) != 1 {
		t.Fatalf("want 1 override, got %d", len(ex.Overrides))
	}
	if got := ex.Overrides[0].Approver; got != "bob@kanz" {
		t.Errorf("approver = %q after a round trip, want bob@kanz — a dual-signed override reads "+
			"as single-signed", got)
	}
	if !ex.Overrides[0].DualSigned() {
		t.Error("DualSigned() is false after a round trip")
	}

	// A single-signed override stores '' and reads back as not-dual-signed.
	if err := es.Override(ctx, "RT:PRICE_TOLERANCE:ICE", pricing.Override{
		Actor: "carol@kanz", Reason: "single", ChosenPrice: rat, At: pgNow,
	}, Claim{}); err != nil {
		t.Fatal(err)
	}
	ex, _, _ = es.Get(ctx, "RT:PRICE_TOLERANCE:ICE")
	single := ex.Overrides[len(ex.Overrides)-1]
	if single.Approver != "" || single.DualSigned() {
		t.Errorf("single-signed override reads approver=%q dual=%v", single.Approver, single.DualSigned())
	}
}

// ===== THE PROPOSAL STORE =====

func TestPostgresProposals_RoundTripAndPending(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	seedException(t, pool, "P1:PRICE_TOLERANCE:ICE")
	ps := NewPostgresProposals(pool)

	prop := proposalFor(t, "prop-1", "P1:PRICE_TOLERANCE:ICE", "alice@kanz", "130")
	if err := ps.Put(ctx, prop); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok, err := ps.Get(ctx, "prop-1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Proposer != "alice@kanz" || got.Subject != "P1:PRICE_TOLERANCE:ICE" {
		t.Errorf("round trip lost the proposal: %+v", got)
	}
	if got.ChosenPrice.RatString() != "130" {
		t.Errorf("chosen price = %s, want 130", got.ChosenPrice.RatString())
	}
	// THE DIGEST MUST SURVIVE EXACTLY. It is what binds the approval to the
	// payload; a digest that changed in storage would refuse every approval, and
	// the obvious fix for that is to stop checking it.
	if got.Digest != prop.Digest {
		t.Errorf("digest changed in storage: %q -> %q", prop.Digest, got.Digest)
	}
	if got.Act != dualcontrol.ActPricingOverride {
		t.Errorf("act = %q", got.Act)
	}
	// And the recomputed digest matches, which is what the approve path does.
	if PayloadDigest(got.Subject, got.Reason, got.ChosenPrice) != got.Digest {
		t.Error("the digest recomputed from the STORED payload does not match the stored digest — " +
			"every approval would be refused as a changed payload")
	}

	pending, err := ps.Pending(ctx, pgNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "prop-1" {
		t.Fatalf("Pending = %d entries, want the one just stored", len(pending))
	}
}

// AN EXPIRED PROPOSAL LEAVES THE PENDING LIST BY ITSELF.
//
// The list is what makes an unapproved act visible; one that keeps expired
// entries reads as outstanding work nobody did.
func TestPostgresProposals_ExpiredProposalsAreNotPending(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	seedException(t, pool, "P2:PRICE_TOLERANCE:ICE")
	ps := NewPostgresProposals(pool)

	if err := ps.Put(ctx, proposalFor(t, "prop-exp", "P2:PRICE_TOLERANCE:ICE", "alice@kanz", "130")); err != nil {
		t.Fatal(err)
	}
	pending, err := ps.Pending(ctx, pgNow.Add(dualcontrol.DefaultTTL+time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("an expired proposal is still listed as pending: %+v", pending)
	}
}

// CLAIM IS ATOMIC, AND THAT IS THE WHOLE REASON IT IS ONE STATEMENT.
//
// Two approvers acting at the same moment both pass every check — different
// people, matching digest, neither expired — and would both apply, appending two
// override records for one decision. Only the DELETE's rows-affected count can
// break the tie, and only the database can enforce it. THIS IS THE TEST THAT
// CANNOT BE WRITTEN AGAINST THE IN-MEMORY STORE: a mutex in one process says
// nothing about two pods.
func TestPostgresProposals_ConcurrentClaimsElectExactlyOneWinner(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	seedException(t, pool, "P3:PRICE_TOLERANCE:ICE")
	ps := NewPostgresProposals(pool)

	if err := ps.Put(ctx, proposalFor(t, "prop-race", "P3:PRICE_TOLERANCE:ICE", "alice@kanz", "130")); err != nil {
		t.Fatal(err)
	}

	const racers = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		won  int
		errs []error
	)
	start := make(chan struct{})
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := ps.Claim(ctx, "prop-race")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if ok {
				won++
			}
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("claim error: %v", err)
	}
	if won != 1 {
		t.Fatalf("%d of %d concurrent claims reported winning, want exactly 1 — %d override records "+
			"would be appended for one decision", won, racers, won)
	}
	if _, ok, err := ps.Get(ctx, "prop-race"); err != nil || ok {
		t.Errorf("the proposal survived the winning claim: ok=%v err=%v", ok, err)
	}
}

// A PROPOSAL BORN EXPIRED CANNOT BE STORED. It would sit in the pending list
// forever looking like work nobody did, and could never be approved.
func TestPostgresProposals_TheDatabaseRefusesAProposalBornExpired(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	seedException(t, pool, "P4:PRICE_TOLERANCE:ICE")

	rat, _ := new(big.Rat).SetString("130")
	_, err := pool.Exec(ctx, `
		INSERT INTO exception_override_proposals
			(proposal_id, exception_id, act, proposer, digest, reason, chosen_price, created_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	`, "born-expired", "P4:PRICE_TOLERANCE:ICE", string(dualcontrol.ActPricingOverride),
		"alice@kanz", "d", "r", rat.RatString(), pgNow, pgNow.Add(-time.Hour))
	if err == nil {
		t.Fatal("Postgres stored a proposal whose expiry precedes its creation")
	}
	if !strings.Contains(err.Error(), "expires_after_creation") {
		t.Errorf("refused, but not by the CHECK constraint: %v", err)
	}
}

// A PROPOSAL WITH NO PROPOSER CANNOT BE STORED.
//
// Every approver differs from an empty proposer, so the self-approval check
// would pass vacuously — the one way this control fails silently rather than
// loudly.
func TestPostgresProposals_TheDatabaseRefusesAProposalWithNoProposer(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	seedException(t, pool, "P5:PRICE_TOLERANCE:ICE")

	for _, proposer := range []string{"", "   "} {
		_, err := pool.Exec(ctx, `
			INSERT INTO exception_override_proposals
				(proposal_id, exception_id, act, proposer, digest, reason, chosen_price, created_at, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, "no-proposer", "P5:PRICE_TOLERANCE:ICE", "PRICING_OVERRIDE",
			proposer, "d", "r", "130", pgNow, pgNow.Add(time.Hour))
		if err == nil {
			t.Errorf("Postgres stored a proposal with proposer %q — every approver would differ "+
				"from it and the self-approval check would pass vacuously", proposer)
		}
	}
}

// A PROPOSAL FOR AN EXCEPTION THAT DOES NOT EXIST IS REFUSED by the foreign key,
// so a pending decision cannot outlive the thing it decides.
func TestPostgresProposals_AProposalNeedsItsException(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	ps := NewPostgresProposals(pool)

	err := ps.Put(ctx, proposalFor(t, "orphan", "NO-SUCH-EXCEPTION", "alice@kanz", "130"))
	if err == nil {
		t.Fatal("a proposal was stored against an exception that does not exist")
	}
	if errors.Is(err, ErrNoProposal) {
		t.Errorf("wrong error class: %v", err)
	}
}

// DELETING AN EXCEPTION TAKES ITS PENDING PROPOSALS WITH IT (ON DELETE CASCADE),
// so a decision cannot be approved into a void.
func TestPostgresProposals_CascadeWithTheException(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	seedException(t, pool, "P6:PRICE_TOLERANCE:ICE")
	ps := NewPostgresProposals(pool)

	if err := ps.Put(ctx, proposalFor(t, "prop-cascade", "P6:PRICE_TOLERANCE:ICE", "alice@kanz", "130")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM exceptions WHERE exception_id = $1`, "P6:PRICE_TOLERANCE:ICE"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ps.Get(ctx, "prop-cascade"); err != nil || ok {
		t.Errorf("the proposal outlived its exception: ok=%v err=%v", ok, err)
	}
}

// ===== THE OVERRIDE AND ITS FACT COMMIT TOGETHER (#410) =====

// ONE TRANSACTION COVERS THE AUDIT ROW AND THE ANNOUNCEMENT.
//
// This is the property the outbox exists for, and it cannot be observed from the
// Go code: the enqueue is a second write inside the same tx, and whether the two
// share a fate is a database question.
//
// Without it there are two windows and both are silent. Publish after commit and
// a crash between them leaves an override the estate never hears about. Publish
// before commit and a rollback leaves a FACT announcing a decision that never
// happened.
func TestPostgresOverride_TheFactIsEnqueuedInTheSameTransaction(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	es := NewPostgresExceptions(pool, "__system__")
	seedException(t, pool, "TX:PRICE_TOLERANCE:ICE")

	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&before); err != nil {
		t.Fatalf("count outbox: %v", err)
	}

	rat, _ := new(big.Rat).SetString("130.25")
	if err := es.Override(ctx, "TX:PRICE_TOLERANCE:ICE", pricing.Override{
		Actor: "alice@kanz", Approver: "bob@kanz", Reason: "vendor confirmed",
		ChosenPrice: rat, At: pgNow,
	}, Claim{}); err != nil {
		t.Fatalf("Override: %v", err)
	}

	var (
		subject, eventType, partitionKey, envelopeTenant string
		published                                        *time.Time
		payload                                          []byte
	)
	if err := pool.QueryRow(ctx, `
		SELECT subject, event_type, partition_key, envelope_tenant_id, published_at, payload
		FROM outbox ORDER BY id DESC LIMIT 1
	`).Scan(&subject, &eventType, &partitionKey, &envelopeTenant, &published, &payload); err != nil {
		t.Fatalf("the override committed but enqueued NO FACT — the audit trail will never hear it: %v", err)
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Errorf("outbox grew by %d, want exactly 1 FACT per override", after-before)
	}
	if subject != SubjectExceptionOverridden || eventType != EventTypeExceptionOverridden {
		t.Errorf("enqueued subject/type = %q/%q", subject, eventType)
	}
	if partitionKey != "TX:PRICE_TOLERANCE:ICE" {
		t.Errorf("partition key = %q, want the exception id", partitionKey)
	}
	if envelopeTenant == "" {
		t.Error("the enqueued FACT carries no envelope tenant — the relay cannot publish it, and it " +
			"will sit in the outbox forever")
	}
	// UNPUBLISHED, because the relay has not run. A row that arrived already
	// marked published would never be sent.
	if published != nil {
		t.Errorf("the FACT was enqueued already marked published at %v", published)
	}

	// AND IT CARRIES BOTH IDENTITIES, read back from the stored bytes rather than
	// from the struct that produced them.
	var msg masterpb.ExceptionOverridden
	if err := proto.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("the stored payload does not unmarshal — the relay could never publish it: %v", err)
	}
	if msg.GetOverride().GetActor() != "alice@kanz" || msg.GetOverride().GetApprover() != "bob@kanz" {
		t.Errorf("the stored FACT names actor=%q approver=%q", msg.GetOverride().GetActor(),
			msg.GetOverride().GetApprover())
	}
}

// A FAILED OVERRIDE ENQUEUES NOTHING.
//
// The rollback must take the FACT with it, or the estate is told about a
// decision that did not happen.
func TestPostgresOverride_AFailedOverrideAnnouncesNothing(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	es := NewPostgresExceptions(pool, "__system__")

	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	rat, _ := new(big.Rat).SetString("130")
	// Unknown exception: the override fails after the transaction has opened.
	if err := es.Override(ctx, "NO-SUCH-EXCEPTION", pricing.Override{
		Actor: "alice@kanz", Reason: "r", ChosenPrice: rat, At: pgNow,
	}, Claim{}); err == nil {
		t.Fatal("an override against an unknown exception succeeded")
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("a FAILED override left %d FACT(s) in the outbox — the estate would be told about a "+
			"decision that never happened", after-before)
	}
}

// LAPSED AND PURGE, AGAINST THE ENGINE THAT ACTUALLY HOLDS THE ROWS (#563).
//
// The memory contract asserts the semantics; only this asserts that the SQL says
// the same thing. The two predicates are inverses spelled in different
// statements — `expires_at > $1` and `expires_at <= $1` — and an off-by-one
// between them is a proposal in neither list, which is the exact silent drop
// this change removes.
func TestPostgresProposals_AnExpiredProposalIsLapsedNotGone(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	seedException(t, pool, "P9:PRICE_TOLERANCE:ICE")
	ps := NewPostgresProposals(pool)

	if err := ps.Put(ctx, proposalFor(t, "prop-lapsed", "P9:PRICE_TOLERANCE:ICE", "alice@kanz", "130")); err != nil {
		t.Fatal(err)
	}
	after := pgNow.Add(dualcontrol.DefaultTTL + time.Hour)

	pending, err := ps.Pending(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	lapsed, err := ps.Lapsed(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("an expired proposal is still pending: %+v", pending)
	}
	if len(lapsed) != 1 || lapsed[0].ID != "prop-lapsed" {
		t.Fatalf("lapsed = %+v, want prop-lapsed. A row in NEITHER list is the silent drop: the "+
			"proposer cannot tell it from one that was never proposed", lapsed)
	}
	if lapsed[0].Proposer != "alice@kanz" {
		t.Errorf("proposer = %q — a lapsed entry that cannot say whose it was tells nobody anything",
			lapsed[0].Proposer)
	}
}

// THE BOUNDARY IS THE TEST. A purge that takes a proposal still inside the
// retention window deletes the record the proposer came back to read; one that
// spares an ancient row is the unbounded growth this exists to end.
func TestPostgresProposals_PurgeTakesOnlyWhatIsPastRetention(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	seedException(t, pool, "P10:PRICE_TOLERANCE:ICE")
	ps := NewPostgresProposals(pool)

	if err := ps.Put(ctx, proposalFor(t, "prop-purge", "P10:PRICE_TOLERANCE:ICE", "alice@kanz", "130")); err != nil {
		t.Fatal(err)
	}
	expiry := pgNow.Add(dualcontrol.DefaultTTL)

	// Not yet past retention: it must survive, and it must still be READABLE,
	// because being listed is the whole point of keeping it.
	n, err := ps.PurgeLapsed(ctx, expiry.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("purged %d proposals inside the retention window — that deletes the record a "+
			"proposer returns to read, and a live proposal is a signature somebody was about to give", n)
	}
	if lapsed, lErr := ps.Lapsed(ctx, expiry.Add(time.Minute)); lErr != nil || len(lapsed) != 1 {
		t.Fatalf("lapsed = %+v (err=%v) after a no-op purge, want the proposal still listed", lapsed, lErr)
	}

	// Past retention: it goes, and the count says so.
	if n, err = ps.PurgeLapsed(ctx, expiry.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("purged %d (err=%v), want 1 — a purge reporting 0 while rows remain is how this "+
			"table grew unnoticed in the first place", n, err)
	}
	if lapsed, lErr := ps.Lapsed(ctx, expiry.Add(2*time.Hour)); lErr != nil || len(lapsed) != 0 {
		t.Fatalf("lapsed = %+v (err=%v) after the purge", lapsed, lErr)
	}
}

// TWO PURGERS RACING PRODUCE ONE OUTCOME. There is no leader election on this
// loop, deliberately: PurgeLapsed is an idempotent DELETE, so replicas racing
// cost a smaller count on the loser rather than a wrong answer. If that stops
// being true the composition root needs a lock, and this is what would say so.
func TestPostgresProposals_ConcurrentPurgesRemoveEachRowOnce(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	seedException(t, pool, "P11:PRICE_TOLERANCE:ICE")
	ps := NewPostgresProposals(pool)

	const rows = 8
	for i := 0; i < rows; i++ {
		id := fmt.Sprintf("prop-race-%d", i)
		if err := ps.Put(ctx, proposalFor(t, id, "P11:PRICE_TOLERANCE:ICE", "alice@kanz", "130")); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := pgNow.Add(dualcontrol.DefaultTTL + time.Hour)

	var wg sync.WaitGroup
	counts := make([]int64, 4)
	for i := range counts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n, err := ps.PurgeLapsed(ctx, cutoff)
			if err != nil {
				t.Errorf("purger %d: %v", i, err)
				return
			}
			counts[i] = n
		}(i)
	}
	wg.Wait()

	var total int64
	for _, n := range counts {
		total += n
	}
	if total != rows {
		t.Errorf("purgers removed %d rows in total, want %d — a row counted twice means the DELETE "+
			"is not the serialisation point this loop assumes it is", total, rows)
	}
	if lapsed, err := ps.Lapsed(ctx, cutoff); err != nil || len(lapsed) != 0 {
		t.Fatalf("lapsed = %+v (err=%v) after concurrent purges", lapsed, err)
	}
}
