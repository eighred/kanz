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
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eighred/kanz/internal/identity"
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

	b, err := os.ReadFile(filepath.Join("..", "..", "services", "identity", "migrations", "0001_identity.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(b)); err != nil {
		t.Fatalf("apply migration: %v", err)
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
