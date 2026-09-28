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

func TestRotationFencesRehashAndCommitsCredentialFreeAudit(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	old, _ := identity.HashCredential("synthetic-original-password")
	replacement, _ := identity.HashCredential("synthetic-replacement-password")
	rehash, _ := identity.HashCredential("synthetic-original-password")
	u, err := st.Redeem(ctx, invite(t, st, "rotation", "alice"), old, now0)
	if err != nil {
		t.Fatal(err)
	}
	a := identity.Administration{Subject: u.Subject, Tenant: u.Tenant, IssuedAt: now0}
	if err = st.RotateCredential(ctx, a, old, replacement, now0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = st.UpdateCredential(ctx, u.Subject, old, rehash, now0.Add(time.Hour)); !errors.Is(err, identity.ErrCredentialMismatch) {
		t.Fatal("late rehash was not fenced", err)
	}
	fresh, err := st.UserBySubject(ctx, u.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Verify(fresh.Credential, "synthetic-original-password") == nil || identity.Verify(fresh.Credential, "synthetic-replacement-password") != nil || fresh.SessionEpoch != 1 || a.Current(fresh) {
		t.Fatal("credential/session fence failed")
	}
	for _, actor := range []identity.Administration{a, {Subject: u.Subject, Tenant: "foreign", IssuedAt: now0}, {Subject: "missing", Tenant: u.Tenant, IssuedAt: now0}} {
		if err = st.RotateCredential(ctx, actor, replacement, rehash, now0); !errors.Is(err, identity.ErrCredentialMismatch) {
			t.Fatal("invalid actor accepted", err)
		}
	}
	var payload string
	if err = auditPool(t, pool, u.Tenant).QueryRow(ctx, `SELECT decision::text FROM identity_access_audit`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, "identity.account.credential.rotate") || !strings.Contains(payload, "account.after") {
		t.Fatal("missing audit evidence")
	}
	for _, secret := range []string{string(old), string(replacement), "synthetic-original-password", "synthetic-replacement-password"} {
		if strings.Contains(payload, secret) {
			t.Fatal("credential material leaked into audit")
		}
	}
}

func TestConcurrentRotationsHaveExactlyOneCommit(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	old, _ := identity.HashCredential("synthetic-original-password")
	replacement, _ := identity.HashCredential("synthetic-replacement-password")
	u, err := st.Redeem(ctx, invite(t, st, "concurrent-rotation", "alice"), old, now0)
	if err != nil {
		t.Fatal(err)
	}
	a := identity.Administration{Subject: u.Subject, Tenant: u.Tenant, IssuedAt: now0}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	start := make(chan struct{})
	for range 8 {
		wg.Go(func() { <-start; results <- st.RotateCredential(ctx, a, old, replacement, now0.Add(time.Second)) })
	}
	close(start)
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, identity.ErrCredentialMismatch) {
			t.Fatal(err)
		}
	}
	var count int
	if err = auditPool(t, pool, u.Tenant).QueryRow(ctx, `SELECT count(*) FROM identity_access_audit`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if wins != 1 || count != 1 {
		t.Fatalf("commits=%d audit=%d", wins, count)
	}
}

func TestRotationAuditFailureRollsBackAndDisabledAccountCannotRotate(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	old, _ := identity.HashCredential("synthetic-original-password")
	replacement, _ := identity.HashCredential("synthetic-replacement-password")
	u, err := st.Redeem(ctx, invite(t, st, "failed-rotation", "alice"), old, now0)
	if err != nil {
		t.Fatal(err)
	}
	a := identity.Administration{Subject: u.Subject, Tenant: u.Tenant, IssuedAt: now0}
	if _, err = pool.Exec(ctx, `CREATE FUNCTION refuse_rotation_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit outage'; END $$; CREATE TRIGGER refuse_rotation_audit BEFORE INSERT ON identity_access_audit FOR EACH STATEMENT EXECUTE FUNCTION refuse_rotation_audit()`); err != nil {
		t.Fatal(err)
	}
	if err = st.RotateCredential(ctx, a, old, replacement, now0.Add(time.Second)); err == nil {
		t.Fatal("audit failure accepted")
	}
	fresh, err := st.UserBySubject(ctx, u.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Credential != old || fresh.SessionEpoch != 0 || fresh.TokensInvalidBefore != nil {
		t.Fatal("failed transaction changed credentials or session fence")
	}
	if _, err = pool.Exec(ctx, `DROP TRIGGER refuse_rotation_audit ON identity_access_audit`); err != nil {
		t.Fatal(err)
	}
	if err = st.SetStatus(ctx, testAdmin, u.Subject, identity.StatusDisabled, now0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	a.SessionEpoch = 1
	if err = st.RotateCredential(ctx, a, old, replacement, now0.Add(time.Hour)); !errors.Is(err, identity.ErrCredentialMismatch) {
		t.Fatal("disabled rotation accepted", err)
	}
}
