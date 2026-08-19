package identity_test

// THE IDENTITY STORE, AGAINST A REAL POSTGRES (#364).
//
// Gated on TEST_POSTGRES_URL. Each test builds its own schema so the suite
// cannot collide with a developer's real one.
//
// WHY A REAL DATABASE AND NOT A FAKE. Two of the guarantees here are not
// properties of the Go code at all — they are properties of the SQL:
//
//   - single use is `WHERE redeemed_at IS NULL` inside a transaction, and the
//     thing being tested is that two concurrent redemptions cannot both win. A
//     fake store with a mutex would pass while proving nothing about the
//     statement that actually runs;
//   - "at most one live invite per subject" is a partial unique index. In Go it
//     is not enforced anywhere.
//
// A double would report success for both.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/revocation"
)

var now0 = time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *identity.Postgres {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL (kanz/test/backing/up.sh) to run the identity store tests")
	}
	ctx := context.Background()

	schema := fmt.Sprintf("identity_test_%d", time.Now().UnixNano())
	boot, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect (bootstrap): %v", err)
	}
	defer boot.Close()
	if _, err := boot.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		drop, derr := pgxpool.New(context.Background(), url)
		if derr != nil {
			t.Errorf("reconnect to drop schema %s: %v — it is now residue", schema, derr)
			return
		}
		defer drop.Close()
		if _, derr := drop.Exec(context.Background(),
			`DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); derr != nil {
			t.Errorf("drop schema %s: %v — it is now residue", schema, derr)
		}
	})

	// EVERY MIGRATION IN ORDER, NOT A NAMED ONE. This used to apply
	// 0001_identity.sql by name, so the day a second migration landed the schema
	// under test silently diverged from the deployed one — and the tests for
	// whatever that migration added would have failed against a column the fixture
	// never created, which reads as a broken feature rather than a stale fixture.
	dir := filepath.Join("..", "..", "services", "identity", "migrations")
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no migrations found under %s — the fixture would build an empty schema and every "+
			"test below would fail for the wrong reason", dir)
	}
	sort.Strings(files)
	for _, f := range files {
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read migration %s: %v", f, rerr)
		}
		if _, eerr := pool.Exec(ctx, string(b)); eerr != nil {
			t.Fatalf("apply migration %s: %v", f, eerr)
		}
	}
	return identity.NewPostgres(pool)
}

// invite stores one invite and returns its raw token.
func invite(t *testing.T, st *identity.Postgres, id, subject string) string {
	t.Helper()
	raw, hash, err := identity.NewInviteToken()
	if err != nil {
		t.Fatalf("NewInviteToken: %v", err)
	}
	inv, err := identity.NewInvite(id, hash, subject, "acme",
		[]string{"kanz-trader"}, []string{"pf-1"}, "user:operator", now0, 0)
	if err != nil {
		t.Fatalf("NewInvite: %v", err)
	}
	if err := st.CreateInvite(context.Background(), inv); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	return raw
}

func TestRedeemCreatesTheAccountTheInviteDescribes(t *testing.T) {
	st := newStore(t)
	raw := invite(t, st, "inv-1", "user:alice")

	cred, err := identity.HashCredential("a good password")
	if err != nil {
		t.Fatalf("HashCredential: %v", err)
	}
	u, err := st.Redeem(context.Background(), raw, cred, now0)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}

	if u.Subject != "user:alice" || u.Tenant != "acme" {
		t.Errorf("account = %s/%s, want user:alice/acme", u.Subject, u.Tenant)
	}
	if len(u.Roles) != 1 || u.Roles[0] != "kanz-trader" {
		t.Errorf("roles = %v, want [kanz-trader] — the claims come from the INVITE, and an "+
			"account that got different ones was provisioned by nobody", u.Roles)
	}
	if !u.Active() {
		t.Errorf("status = %q, want active", u.Status)
	}

	// And it is durable, with a credential that verifies.
	loaded, err := st.UserBySubject(context.Background(), "user:alice")
	if err != nil {
		t.Fatalf("UserBySubject: %v", err)
	}
	if err := identity.Verify(loaded.Credential, "a good password"); err != nil {
		t.Fatalf("the stored credential does not verify: %v", err)
	}
	if err := identity.Verify(loaded.Credential, "the wrong password"); err == nil {
		t.Fatal("the stored credential verified the WRONG password")
	}
}

// TWO CONCURRENT REDEMPTIONS: EXACTLY ONE WINS.
//
// This is the test a fake store cannot do. Single use is `WHERE redeemed_at IS
// NULL` inside a transaction; a load-check-save in Go would let both callers
// pass the check before either wrote, and produce either two accounts or one
// account whose credential is whichever committed last.
func TestOnlyOneOfTwoConcurrentRedemptionsWins(t *testing.T) {
	st := newStore(t)
	raw := invite(t, st, "inv-race", "user:racer")

	cred, err := identity.HashCredential("pw")
	if err != nil {
		t.Fatalf("HashCredential: %v", err)
	}

	const attempts = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		errs  []error
		start = make(chan struct{})
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together, to make the race real
			_, err := st.Redeem(context.Background(), raw, cred, now0)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else {
				errs = append(errs, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d of %d concurrent redemptions succeeded, want exactly 1.\n\n"+
			"An invite that can be redeemed twice is a shared password. More than one winner "+
			"means the single-use check is a read-then-write and not the conditional UPDATE.",
			wins, attempts)
	}
	if len(errs) != attempts-1 {
		t.Errorf("losers = %d, want %d", len(errs), attempts-1)
	}
}

func TestARedeemedInviteCannotBeUsedAgain(t *testing.T) {
	st := newStore(t)
	raw := invite(t, st, "inv-2", "user:bob")
	cred, _ := identity.HashCredential("pw")

	if _, err := st.Redeem(context.Background(), raw, cred, now0); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}
	_, err := st.Redeem(context.Background(), raw, cred, now0.Add(time.Minute))
	if err != identity.ErrInviteNotFound {
		t.Fatalf("second Redeem = %v, want ErrInviteNotFound.\n\n"+
			"The three refusal states are collapsed on purpose: this caller is unauthenticated "+
			"and holds only a link, so distinguishing them confirms an account was offered.", err)
	}
}

func TestAnExpiredInviteIsRefused(t *testing.T) {
	st := newStore(t)
	raw := invite(t, st, "inv-3", "user:carol")
	cred, _ := identity.HashCredential("pw")

	after := now0.Add(identity.DefaultInviteTTL + time.Hour)
	if _, err := st.Redeem(context.Background(), raw, cred, after); err != identity.ErrInviteNotFound {
		t.Fatalf("Redeem after expiry = %v, want ErrInviteNotFound", err)
	}
	// And it is still unredeemed — a refused attempt must not burn the invite,
	// or an expired-link click would destroy the operator's ability to see it.
	list, err := st.InvitesFor(context.Background(), "acme")
	if err != nil {
		t.Fatalf("InvitesFor: %v", err)
	}
	if len(list) != 1 || list[0].RedeemedAt != nil {
		t.Fatalf("invite after a refused redemption = %+v, want still unredeemed", list[0])
	}
}

// AT MOST ONE LIVE INVITE PER SUBJECT, enforced by the database.
//
// Two live invites are two authorities racing to become one account, and the
// one redeemed second silently loses — including when an operator issued a
// CORRECTED invite and the stale one gets used.
func TestASecondLiveInviteForTheSameSubjectIsRefused(t *testing.T) {
	st := newStore(t)
	invite(t, st, "inv-a", "user:dave")

	_, hash, _ := identity.NewInviteToken()
	second, err := identity.NewInvite("inv-b", hash, "user:dave", "acme",
		[]string{"kanz-operator"}, nil, "user:operator", now0, 0)
	if err != nil {
		t.Fatalf("NewInvite: %v", err)
	}
	if err := st.CreateInvite(context.Background(), second); err == nil {
		t.Fatal("a second LIVE invite for user:dave was accepted.\n\n" +
			"Note the roles differ — kanz-operator vs kanz-trader. Whichever is redeemed decides " +
			"the account's authority, and the operator who issued the other one is never told.")
	}
}

// A REDEEMED SUBJECT CAN BE RE-INVITED — the partial index covers live invites
// only, so re-provisioning after an account is removed is possible.
func TestASubjectCanBeReInvitedOnceTheFirstIsRedeemed(t *testing.T) {
	st := newStore(t)
	raw := invite(t, st, "inv-x", "user:erin")
	cred, _ := identity.HashCredential("pw")
	if _, err := st.Redeem(context.Background(), raw, cred, now0); err != nil {
		t.Fatalf("Redeem: %v", err)
	}

	_, hash, _ := identity.NewInviteToken()
	again, err := identity.NewInvite("inv-y", hash, "user:erin", "acme",
		[]string{"kanz-user"}, nil, "user:operator", now0, 0)
	if err != nil {
		t.Fatalf("NewInvite: %v", err)
	}
	if err := st.CreateInvite(context.Background(), again); err != nil {
		t.Fatalf("re-inviting a redeemed subject failed: %v — the index must cover LIVE "+
			"invites only, or an account can never be re-provisioned", err)
	}
}

func TestAnUnknownSubjectIsNotFound(t *testing.T) {
	st := newStore(t)
	if _, err := st.UserBySubject(context.Background(), "user:nobody"); err != identity.ErrUserNotFound {
		t.Fatalf("UserBySubject = %v, want ErrUserNotFound", err)
	}
}

func TestUpdateCredentialRewritesTheHash(t *testing.T) {
	st := newStore(t)
	raw := invite(t, st, "inv-4", "user:frank")
	first, _ := identity.HashCredential("first")
	if _, err := st.Redeem(context.Background(), raw, first, now0); err != nil {
		t.Fatalf("Redeem: %v", err)
	}

	second, _ := identity.HashCredential("second")
	if err := st.UpdateCredential(context.Background(), "user:frank", second, now0.Add(time.Hour)); err != nil {
		t.Fatalf("UpdateCredential: %v", err)
	}
	u, err := st.UserBySubject(context.Background(), "user:frank")
	if err != nil {
		t.Fatalf("UserBySubject: %v", err)
	}
	if err := identity.Verify(u.Credential, "second"); err != nil {
		t.Fatalf("the new credential does not verify: %v", err)
	}
	if err := identity.Verify(u.Credential, "first"); err == nil {
		t.Fatal("the OLD credential still verifies — a rotation that leaves the previous " +
			"password working is not a rotation")
	}
	if err := st.UpdateCredential(context.Background(), "user:nobody", second, now0); err != identity.ErrUserNotFound {
		t.Errorf("UpdateCredential for an unknown subject = %v, want ErrUserNotFound", err)
	}
}

// SetStatus IS THE WRITE THAT MAKES StatusDisabled REACHABLE (#525).
//
// Gated on TEST_POSTGRES_URL like every test in this file, so it SKIPS on a box
// without a database — and everything it proves is a property of the statement,
// which is why it lives here and not beside a fake:
//
//   - the row actually changes, and updated_at moves with it;
//   - the disabled account is then refused by Signer.Mint, the last point that
//     sees a User;
//   - a subject that matched no row is ErrUserNotFound and NOT a silent success.
func TestSetStatusDisablesAnAccountAndMintThenRefusesIt(t *testing.T) {
	st := newStore(t)
	raw := invite(t, st, "inv-disable", "user:grace")
	cred, _ := identity.HashCredential("pw")
	if _, err := st.Redeem(context.Background(), raw, cred, now0); err != nil {
		t.Fatalf("Redeem: %v", err)
	}

	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	signer, err := identity.NewSigner(key, "https://identity.test", "kanz", time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	before, err := st.UserBySubject(context.Background(), "user:grace")
	if err != nil {
		t.Fatalf("UserBySubject: %v", err)
	}
	if !before.Active() {
		t.Fatalf("precondition: a freshly redeemed account is %q, want active", before.Status)
	}
	if _, _, err := signer.Mint(before); err != nil {
		t.Fatalf("precondition: Mint refused an active account: %v", err)
	}

	later := now0.Add(2 * time.Hour)
	if err := st.SetStatus(context.Background(), "user:grace", identity.StatusDisabled, later); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	after, err := st.UserBySubject(context.Background(), "user:grace")
	if err != nil {
		t.Fatalf("UserBySubject: %v", err)
	}
	if after.Active() {
		t.Fatalf("status = %q after a disable, want disabled — the row did not change, and every "+
			"caller above this reports a lockout that did not happen", after.Status)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("updated_at = %s, was %s — a status change that leaves the timestamp behind is "+
			"invisible to anything reconciling the table against the audit log",
			after.UpdatedAt, before.UpdatedAt)
	}
	if _, _, err := signer.Mint(after); err == nil {
		t.Fatal("Mint issued a token for a disabled account — the enforcement point the disable " +
			"exists to reach")
	}

	// AND BACK. Without the inverse, a mistaken disable needs the hand-run UPDATE
	// this method was written to eliminate.
	if err := st.SetStatus(context.Background(), "user:grace", identity.StatusActive, later.Add(time.Hour)); err != nil {
		t.Fatalf("SetStatus (re-enable): %v", err)
	}
	back, err := st.UserBySubject(context.Background(), "user:grace")
	if err != nil {
		t.Fatalf("UserBySubject: %v", err)
	}
	if !back.Active() {
		t.Fatalf("status = %q after a re-enable, want active", back.Status)
	}
}

// THE DISABLE STAMPS THE REVOCATION MARK, IN THE SAME STATEMENT (#532).
//
// Everything here is a property of that one UPDATE, which is why it cannot be
// proven against a fake:
//
//   - a disable publishes a mark, so the gateway can refuse the token the
//     account already holds instead of waiting for the next login;
//   - an ENABLE DOES NOT CLEAR IT. Clearing would resurrect the exact token the
//     disable killed;
//   - a second disable only moves the mark FORWARD, so a clock that steps back
//     cannot narrow a revocation that has already been published.
func TestDisablingAnAccountPublishesARevocationMarkAndEnablingDoesNotClearIt(t *testing.T) {
	st := newStore(t)
	raw := invite(t, st, "inv-revoke", "user:hank")
	cred, _ := identity.HashCredential("pw")
	if _, err := st.Redeem(context.Background(), raw, cred, now0); err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	ctx := context.Background()

	// AN ACTIVE ACCOUNT IS NOT IN THE FEED. A denylist that carried every account
	// would be the size of the user table and would refuse everybody the moment
	// the comparison was wrong.
	entries, err := st.Revocations(ctx)
	if err != nil {
		t.Fatalf("Revocations: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a freshly redeemed account is already in the revocation feed: %+v", entries)
	}

	disabledAt := now0.Add(2 * time.Hour)
	if err := st.SetStatus(ctx, "user:hank", identity.StatusDisabled, disabledAt); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	entries, err = st.Revocations(ctx)
	if err != nil {
		t.Fatalf("Revocations: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the feed has %d entries after a disable, want 1 — the gateway would go on "+
			"honouring this account's outstanding token", len(entries))
	}
	if entries[0].SubjectHash != revocation.HashSubject("user:hank") {
		t.Fatalf("the feed names %q; the gateway looks up %q and would never match",
			entries[0].SubjectHash, revocation.HashSubject("user:hank"))
	}
	if entries[0].SubjectHash == "user:hank" {
		t.Fatal("the feed carries the subject in plaintext")
	}
	if got := time.Unix(entries[0].NotBefore, 0).UTC(); !got.Equal(disabledAt) {
		t.Errorf("not_before = %s, want %s — the mark must be the instant of the disable, or the "+
			"gateway refuses the wrong set of tokens", got, disabledAt)
	}

	// RE-ENABLE KEEPS THE MARK.
	if err := st.SetStatus(ctx, "user:hank", identity.StatusActive, disabledAt.Add(time.Hour)); err != nil {
		t.Fatalf("SetStatus (re-enable): %v", err)
	}
	entries, err = st.Revocations(ctx)
	if err != nil {
		t.Fatalf("Revocations: %v", err)
	}
	if len(entries) != 1 || time.Unix(entries[0].NotBefore, 0).UTC() != disabledAt {
		t.Fatalf("re-enabling cleared or moved the revocation mark: %+v. The token the disable was "+
			"meant to kill would work again", entries)
	}

	// A LATER DISABLE MOVES IT FORWARD.
	again := disabledAt.Add(3 * time.Hour)
	if err := st.SetStatus(ctx, "user:hank", identity.StatusDisabled, again); err != nil {
		t.Fatalf("SetStatus (second disable): %v", err)
	}
	entries, _ = st.Revocations(ctx)
	if len(entries) != 1 || time.Unix(entries[0].NotBefore, 0).UTC() != again {
		t.Fatalf("the second disable did not advance the mark: %+v", entries)
	}

	// AND AN EARLIER ONE DOES NOT MOVE IT BACK. GREATEST() is what makes a
	// stepped-back clock, or a replayed request, unable to narrow a revocation
	// that has already been published to every gateway.
	if err := st.SetStatus(ctx, "user:hank", identity.StatusDisabled, disabledAt); err != nil {
		t.Fatalf("SetStatus (backdated): %v", err)
	}
	entries, _ = st.Revocations(ctx)
	if len(entries) != 1 || time.Unix(entries[0].NotBefore, 0).UTC() != again {
		t.Fatalf("a backdated disable narrowed the revocation window: %+v", entries)
	}
}

// ZERO ROWS AFFECTED IS AN ERROR, NOT A SUCCESS.
//
// A disable that matched no subject is the exact failure this method exists to
// remove: the operator is told the account is locked out, the audit record says
// it was, and nothing happened. A typo'd subject must not be indistinguishable
// from a completed disable.
func TestSetStatusForAnUnknownSubjectIsNotFound(t *testing.T) {
	st := newStore(t)
	err := st.SetStatus(context.Background(), "user:nobody", identity.StatusDisabled, now0)
	if !errors.Is(err, identity.ErrUserNotFound) {
		t.Fatalf("SetStatus for an unknown subject = %v, want ErrUserNotFound", err)
	}
}

// AN UNKNOWN STATUS IS REFUSED IN GO, BEFORE THE STATEMENT RUNS.
//
// The column's CHECK constraint would refuse it too — this asserts that the
// caller gets ErrUnknownStatus rather than an opaque *pgconn.PgError it cannot
// tell from a dead pool, AND that the row is left alone, which is the part a
// constraint violation inside a larger statement would not guarantee.
func TestSetStatusRefusesAnUnknownStatusWithoutTouchingTheRow(t *testing.T) {
	st := newStore(t)
	raw := invite(t, st, "inv-badstatus", "user:heidi")
	cred, _ := identity.HashCredential("pw")
	if _, err := st.Redeem(context.Background(), raw, cred, now0); err != nil {
		t.Fatalf("Redeem: %v", err)
	}

	err := st.SetStatus(context.Background(), "user:heidi", identity.Status("suspended"), now0.Add(time.Hour))
	if !errors.Is(err, identity.ErrUnknownStatus) {
		t.Fatalf("SetStatus with an unknown status = %v, want ErrUnknownStatus — a constraint "+
			"violation reaches the caller as a database error indistinguishable from a "+
			"connection fault", err)
	}
	u, uerr := st.UserBySubject(context.Background(), "user:heidi")
	if uerr != nil {
		t.Fatalf("UserBySubject: %v", uerr)
	}
	if !u.Active() {
		t.Errorf("status = %q after a REFUSED write, want active — the refusal happened after the "+
			"statement, not before it", u.Status)
	}
	if !u.UpdatedAt.Equal(now0) {
		t.Errorf("updated_at moved to %s on a refused write", u.UpdatedAt)
	}
}

// THE UNKNOWN-STATUS REFUSAL HAPPENS BEFORE THE POOL IS TOUCHED, AND THIS TEST
// NEEDS NO DATABASE TO PROVE IT (#525).
//
// The store is built over a NIL pool. If SetStatus reached the Exec — which is
// what "let the CHECK constraint catch it" would mean — this panics on a nil
// dereference instead of returning. So the assertion is not only "an error came
// back", it is "the statement never ran", which is the property that makes the
// error classifiable: a constraint violation arrives as an opaque database error
// a caller cannot tell from a dead pool, and would be reported as an outage.
//
// It is UNGATED on purpose. Every other SetStatus test skips without
// TEST_POSTGRES_URL, and a guarantee whose only proof skips on the box it is
// developed on is a guarantee nothing checks.
func TestSetStatusRefusesAnUnknownStatusWithoutReachingTheDatabase(t *testing.T) {
	st := identity.NewPostgres(nil)
	err := st.SetStatus(context.Background(), "user:anyone", identity.Status("suspended"), now0)
	if !errors.Is(err, identity.ErrUnknownStatus) {
		t.Fatalf("SetStatus over a nil pool = %v, want ErrUnknownStatus", err)
	}
}
