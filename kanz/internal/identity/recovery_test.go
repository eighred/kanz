package identity_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
)

func verifiedMailbox(t *testing.T, st *identity.Postgres) (*identity.User, identity.Hash) {
	t.Helper()
	ctx := context.Background()
	old, err := identity.HashCredential("synthetic-recovery-original")
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.Redeem(ctx, invite(t, st, "recovery-user", "recovery-subject"), old, now0)
	if err != nil {
		t.Fatal(err)
	}
	a := identity.Administration{Subject: u.Subject, Tenant: u.Tenant, IssuedAt: now0}
	if err = st.EnrollMailbox(ctx, a, old, "person@example.test", now0); err != nil {
		t.Fatal(err)
	}
	m, err := st.ClaimMail(ctx, now0)
	if err != nil || m == nil {
		t.Fatal("missing verification delivery", err)
	}
	if m.Purpose != "verify" || m.Address != "person@example.test" {
		t.Fatal("wrong delivery authority")
	}
	if err = st.FinishMail(ctx, m, true, now0); err != nil {
		t.Fatal(err)
	}
	if err = st.ConsumeChallenge(ctx, m.Token, "recover", old, now0); !errors.Is(err, identity.ErrRecovery) {
		t.Fatal("verification used as recovery", err)
	}
	if err = st.ConsumeChallenge(ctx, m.Token, "verify", "", now0); err != nil {
		t.Fatal(err)
	}
	if err = st.ConsumeChallenge(ctx, m.Token, "verify", "", now0); !errors.Is(err, identity.ErrRecovery) {
		t.Fatal("proof reused", err)
	}
	return u, old
}

func TestRecoverySingleUseRevokesSessionsAndFencesLateRehash(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	u, old := verifiedMailbox(t, st)
	if err := st.RequestRecovery(ctx, u.Subject, now0); err != nil {
		t.Fatal(err)
	}
	m, err := st.ClaimMail(ctx, now0)
	if err != nil || m == nil {
		t.Fatal(err)
	}
	if err = st.FinishMail(ctx, m, true, now0); err != nil {
		t.Fatal(err)
	}
	replacement, err := identity.HashCredential("synthetic-recovered-password")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Go(func() { results <- st.ConsumeChallenge(ctx, m.Token, "recover", replacement, now0.Add(time.Second)) })
	}
	wg.Wait()
	close(results)
	wins := 0
	for e := range results {
		if e == nil {
			wins++
		} else if !errors.Is(e, identity.ErrRecovery) {
			t.Fatal(e)
		}
	}
	if wins != 1 {
		t.Fatalf("recovery commits=%d", wins)
	}
	fresh, err := st.UserBySubject(ctx, u.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.SessionEpoch != u.SessionEpoch+1 || fresh.Credential != replacement || fresh.TokensInvalidBefore == nil {
		t.Fatal("missing atomic credential/session fence")
	}
	if err = st.UpdateCredential(ctx, u.Subject, old, old, now0.Add(time.Minute)); !errors.Is(err, identity.ErrCredentialMismatch) {
		t.Fatal("late rehash restored old credential")
	}
	var audit string
	if err = auditPool(t, pool, u.Tenant).QueryRow(ctx, `SELECT string_agg(decision::text,'') FROM identity_access_audit`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(audit, "identity.account.mailbox.recover") {
		t.Fatal("missing recovery audit")
	}
	for _, secret := range []string{m.Token, identity.InviteTokenHash(m.Token), string(old), string(replacement)} {
		if strings.Contains(audit, secret) {
			t.Fatal("secret in audit")
		}
	}
}

func TestRecoveryAuditFailureRollsBackProofAndCredential(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	u, old := verifiedMailbox(t, st)
	if err := st.RequestRecovery(ctx, u.Subject, now0); err != nil {
		t.Fatal(err)
	}
	m, err := st.ClaimMail(ctx, now0)
	if err != nil || m == nil {
		t.Fatal(err)
	}
	replacement, err := identity.HashCredential("synthetic-recovered-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `CREATE FUNCTION refuse_recovery_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit outage'; END $$; CREATE TRIGGER refuse_recovery_audit BEFORE INSERT ON identity_access_audit FOR EACH STATEMENT EXECUTE FUNCTION refuse_recovery_audit()`); err != nil {
		t.Fatal(err)
	}
	if err = st.ConsumeChallenge(ctx, m.Token, "recover", replacement, now0); err == nil {
		t.Fatal("audit failure accepted")
	}
	fresh, err := st.UserBySubject(ctx, u.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Credential != old || fresh.SessionEpoch != 0 {
		t.Fatal("audit failure changed authority")
	}
	if _, err = pool.Exec(ctx, `DROP TRIGGER refuse_recovery_audit ON identity_access_audit`); err != nil {
		t.Fatal(err)
	}
	if err = st.ConsumeChallenge(ctx, m.Token, "recover", replacement, now0); err != nil {
		t.Fatal("rollback burned proof", err)
	}
}

func TestRecoveryDeliveryRetryReplacesTokenAndFencesWorker(t *testing.T) {
	st, _ := newStorePool(t)
	ctx := context.Background()
	u, _ := verifiedMailbox(t, st)
	if err := st.RequestRecovery(ctx, u.Subject, now0); err != nil {
		t.Fatal(err)
	}
	first, err := st.ClaimMail(ctx, now0)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	if other, e := st.ClaimMail(ctx, now0); e != nil || other != nil {
		t.Fatal("duplicate lease", e)
	}
	second, err := st.ClaimMail(ctx, now0.Add(31*time.Second))
	if err != nil || second == nil {
		t.Fatal(err)
	}
	if err = st.FinishMail(ctx, first, true, now0); err != nil {
		t.Fatal(err)
	}
	replacement, err := identity.HashCredential("synthetic-recovered-password")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.ConsumeChallenge(ctx, first.Token, "recover", replacement, now0); !errors.Is(err, identity.ErrRecovery) {
		t.Fatal("old retry token accepted", err)
	}
	if err = st.ConsumeChallenge(ctx, second.Token, "recover", replacement, now0.Add(32*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryDisabledExpiredAndStaleEpochRefuse(t *testing.T) {
	for _, reason := range []string{"disabled", "expired", "rotated"} {
		t.Run(reason, func(t *testing.T) {
			st, _ := newStorePool(t)
			ctx := context.Background()
			u, old := verifiedMailbox(t, st)
			if err := st.RequestRecovery(ctx, u.Subject, now0); err != nil {
				t.Fatal(err)
			}
			m, err := st.ClaimMail(ctx, now0)
			if err != nil || m == nil {
				t.Fatal(err)
			}
			replacement, err := identity.HashCredential("synthetic-recovered-password")
			if err != nil {
				t.Fatal(err)
			}
			at := now0.Add(time.Second)
			switch reason {
			case "disabled":
				err = st.SetStatus(ctx, testAdmin, u.Subject, identity.StatusDisabled, at)
			case "expired":
				at = now0.Add(identity.ChallengeTTL)
			case "rotated":
				err = st.RotateCredential(ctx, identity.Administration{Subject: u.Subject, Tenant: u.Tenant, IssuedAt: now0}, old, replacement, at)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = st.ConsumeChallenge(ctx, m.Token, "recover", replacement, at); !errors.Is(err, identity.ErrRecovery) {
				t.Fatal("invalid proof accepted", err)
			}
		})
	}
}

func TestMailboxChangeRetiresRecoveryAndCooldownIsExplicit(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	u, old := verifiedMailbox(t, st)
	if err := st.RequestRecovery(ctx, u.Subject, now0); err != nil {
		t.Fatal(err)
	}
	reset, err := st.ClaimMail(ctx, now0)
	if err != nil || reset == nil {
		t.Fatal(err)
	}
	if err = st.FinishMail(ctx, reset, true, now0); err != nil {
		t.Fatal(err)
	}
	a := identity.Administration{Subject: u.Subject, Tenant: u.Tenant, IssuedAt: now0}
	at := now0.Add(time.Minute)
	if err = st.EnrollMailbox(ctx, a, old, "replacement@example.test", at); err != nil {
		t.Fatal(err)
	}
	if err = st.EnrollMailbox(ctx, a, old, "other@example.test", at); !errors.Is(err, identity.ErrRecoveryCooldown) {
		t.Fatal("cooldown silently accepted a different destination")
	}
	proof, err := st.ClaimMail(ctx, at)
	if err != nil || proof == nil {
		t.Fatal(err)
	}
	if proof.Purpose != "verify" {
		t.Fatal("missing verification command")
	}
	if err = st.ConsumeChallenge(ctx, proof.Token, "verify", "", at); err != nil {
		t.Fatal(err)
	}
	if err = st.ConsumeChallenge(ctx, reset.Token, "recover", old, at); !errors.Is(err, identity.ErrRecovery) {
		t.Fatal("old mailbox retained recovery authority")
	}
	status, err := st.MailboxStatus(ctx, a, at)
	if err != nil || status.Address != "replacement@example.test" {
		t.Fatal("incorrect mailbox projection", err)
	}
	var digest string
	if err = pool.QueryRow(ctx, `SELECT COALESCE(token_hash,'') FROM identity_mail_challenges WHERE subject=$1 AND purpose='recover'`, u.Subject).Scan(&digest); err != nil || digest != "" {
		t.Fatal("mailbox change retained reset digest", err)
	}
}

func TestAbandonedFinalDeliveryAttemptTerminates(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	u, _ := verifiedMailbox(t, st)
	if err := st.RequestRecovery(ctx, u.Subject, now0); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		m, err := st.ClaimMail(ctx, now0.Add(time.Duration(attempt-1)*31*time.Second))
		if err != nil || m == nil || m.Attempt != attempt {
			t.Fatal("unexpected retry", err)
		}
	}
	if m, err := st.ClaimMail(ctx, now0.Add(94*time.Second)); err != nil || m != nil {
		t.Fatal("unbounded retry", err)
	}
	var state string
	var digest *string
	if err := pool.QueryRow(ctx, `SELECT state,token_hash FROM identity_mail_challenges WHERE subject=$1 AND purpose='recover'`, u.Subject).Scan(&state, &digest); err != nil || state != "failed" || digest != nil {
		t.Fatal("abandoned final attempt not retired", err)
	}
}
