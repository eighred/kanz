package identity_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
)

func administrationAccount(t *testing.T, st *identity.Postgres, subject, tenant string, roles []string) identity.Administration {
	t.Helper()
	raw, hash, err := identity.NewInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	inv, err := identity.NewInvite(subject, hash, subject, tenant, roles, nil, "test:bootstrap", now0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInvite(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Redeem(context.Background(), raw, "test-only-hash", now0); err != nil {
		t.Fatal(err)
	}
	return identity.Administration{Subject: subject, Tenant: tenant, IssuedAt: now0}
}

func TestAdministrationConcurrentMutualDisablePreservesOneAdministrator(t *testing.T) {
	for attempt := range 8 {
		t.Run(fmt.Sprint(attempt), func(t *testing.T) {
			st := newStore(t)
			peer := administrationAccount(t, st, "test:peer", "acme", []string{identity.AdminRole})
			start := make(chan struct{})
			results := make(chan error, 2)
			for _, pair := range [][2]identity.Administration{{testAdmin, peer}, {peer, testAdmin}} {
				go func() {
					<-start
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					results <- st.SetStatus(ctx, pair[0], pair[1].Subject, identity.StatusDisabled, now0.Add(time.Second))
				}()
			}
			close(start)
			wins := 0
			for range 2 {
				if err := <-results; err == nil {
					wins++
				} else if !errors.Is(err, identity.ErrAdminAuthority) {
					t.Fatal(err)
				}
			}
			if wins != 1 {
				t.Fatalf("successful disables = %d, want one", wins)
			}
			active := 0
			for _, subject := range []string{testAdmin.Subject, peer.Subject} {
				u, err := st.UserBySubject(context.Background(), subject)
				if err != nil {
					t.Fatal(err)
				}
				if u.Active() {
					active++
				}
			}
			if active != 1 {
				t.Fatalf("remaining administrators = %d", active)
			}
			marks, err := st.Revocations(context.Background())
			if err != nil || len(marks) != 1 {
				t.Fatalf("revocations = %v, %v", marks, err)
			}
		})
	}
}

func TestAdministrationRefusalsLeaveStatusAndRevocationUntouched(t *testing.T) {
	st := newStore(t)
	other := administrationAccount(t, st, "test:other", "other-tenant", []string{identity.AdminRole})
	trader := administrationAccount(t, st, "test:trader", "acme", []string{"kanz-trader"})
	for _, tc := range []struct {
		name   string
		actor  identity.Administration
		target string
		want   error
	}{
		{"last administrator self-disable", testAdmin, testAdmin.Subject, identity.ErrLastAdmin},
		{"foreign target", testAdmin, other.Subject, identity.ErrUserNotFound},
		{"forged tenant", identity.Administration{Subject: testAdmin.Subject, Tenant: other.Tenant}, other.Subject, identity.ErrAdminAuthority},
		{"infrastructure or trader is not admin", trader, testAdmin.Subject, identity.ErrAdminAuthority},
		{"missing actor", identity.Administration{}, testAdmin.Subject, identity.ErrAdminAuthority},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := st.SetStatus(context.Background(), tc.actor, tc.target, identity.StatusDisabled, now0); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			u, err := st.UserBySubject(context.Background(), tc.target)
			if err != nil || !u.Active() {
				t.Fatalf("refused write changed account: %v %v", u, err)
			}
		})
	}
	marks, err := st.Revocations(context.Background())
	if err != nil || len(marks) != 0 {
		t.Fatalf("refusal emitted revocations: %v %v", marks, err)
	}
}

func TestAdministrationOldTokenCannotActAfterReenable(t *testing.T) {
	st := newStore(t)
	peer := administrationAccount(t, st, "test:peer", "acme", []string{identity.AdminRole})
	ctx := context.Background()
	for _, status := range []identity.Status{identity.StatusDisabled, identity.StatusActive} {
		if err := st.SetStatus(ctx, testAdmin, peer.Subject, status, now0.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetStatus(ctx, peer, testAdmin.Subject, identity.StatusDisabled, now0.Add(2*time.Second)); !errors.Is(err, identity.ErrAdminAuthority) {
		t.Fatalf("old token: %v", err)
	}
	peer.IssuedAt = now0.Add(2 * time.Second)
	if err := st.SetStatus(ctx, peer, testAdmin.Subject, identity.StatusDisabled, now0.Add(3*time.Second)); err != nil {
		t.Fatalf("fresh token: %v", err)
	}
}

func TestAdministrationInviteRacingDisableIsSerialized(t *testing.T) {
	st := newStore(t)
	peer := administrationAccount(t, st, "test:peer", "acme", []string{identity.AdminRole})
	ctx := context.Background()
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for n := range 12 {
		wg.Go(func() {
			<-start
			inv, err := identity.NewInvite(fmt.Sprint(n), fmt.Sprint(n), fmt.Sprintf("test:invite-%d", n), "acme", []string{identity.AdminRole}, nil, peer.Subject, now0, 0)
			if err == nil {
				err = st.CreateInviteAs(ctx, peer, inv)
			}
			errs <- err
		})
	}
	close(start)
	if err := st.SetStatus(ctx, testAdmin, peer.Subject, identity.StatusDisabled, now0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, identity.ErrAdminAuthority) {
			t.Fatal(err)
		}
	}
	inv, err := identity.NewInvite("after", "after", "test:after", "acme", []string{identity.AdminRole}, nil, peer.Subject, now0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInviteAs(ctx, peer, inv); !errors.Is(err, identity.ErrAdminAuthority) {
		t.Fatalf("disabled administrator created invite: %v", err)
	}
	inv.Tenant = "other-tenant"
	if err := st.CreateInviteAs(ctx, testAdmin, inv); !errors.Is(err, identity.ErrAdminAuthority) {
		t.Fatalf("cross tenant invite: %v", err)
	}
}

func TestAdministratorRolesCannotCombineOperationalAuthority(t *testing.T) {
	for _, role := range []string{"kanz-operator", "kanz-trader", "kanz-approver", "custom-sre"} {
		if err := identity.ValidateAdminRoles([]string{identity.AdminRole, role}); !errors.Is(err, identity.ErrAdminRoleCombination) {
			t.Fatalf("accepted %s: %v", role, err)
		}
	}
	if err := identity.ValidateAdminRoles([]string{identity.AdminRole, "kanz-user"}); err != nil {
		t.Fatal(err)
	}
}

func TestAdministrationRechecksActorAfterWaitingForTenantLock(t *testing.T) {
	st, pool := newStorePool(t)
	peer := administrationAccount(t, st, "test:peer", "acme", []string{identity.AdminRole})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('identity-administration:acme', 0))`); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- st.SetStatus(ctx, peer, testAdmin.Subject, identity.StatusDisabled, now0) }()
	// Observe actual PostgreSQL lock contention, not merely goroutine scheduling.
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
            WHERE application_name = current_setting('application_name')
            AND wait_event_type = 'Lock' AND wait_event = 'advisory')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("mutation bypassed held tenant lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE identity_users SET roles = ARRAY['kanz-user'] WHERE subject=$1`, peer.Subject); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, identity.ErrAdminAuthority) {
		t.Fatalf("stale authority after lock wait: %v", err)
	}
	u, err := st.UserBySubject(ctx, testAdmin.Subject)
	if err != nil || !u.Active() {
		t.Fatalf("administrator lost: %v %v", u, err)
	}
}

func TestAdministrationUnredeemedInviteDoesNotReplaceLastAdministrator(t *testing.T) {
	st := newStore(t)
	inv, err := identity.NewInvite("pending", "pending-hash", "test:pending", "acme", []string{identity.AdminRole}, nil, testAdmin.Subject, now0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInviteAs(context.Background(), testAdmin, inv); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus(context.Background(), testAdmin, testAdmin.Subject, identity.StatusDisabled, now0); !errors.Is(err, identity.ErrLastAdmin) {
		t.Fatalf("pending invitation counted as an administrator: %v", err)
	}
	administrationAccount(t, st, "test:peer", "acme", []string{identity.AdminRole})
	if err := st.SetStatus(context.Background(), testAdmin, testAdmin.Subject, identity.StatusDisabled, now0); !errors.Is(err, identity.ErrSelfDisable) {
		t.Fatalf("self-disable with replacement: %v", err)
	}
}

func TestAdministrationLegacyMixedInviteCannotBeRedeemed(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	raw, hash, err := identity.NewInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity_invites
        (id,token_hash,subject,tenant_id,roles,created_by,created_at,expires_at)
        VALUES ('legacy',$1,'test:legacy','acme',ARRAY['kanz-identity-admin','kanz-operator'],'test:bootstrap',$2,$3)`, hash, now0, now0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Redeem(ctx, raw, "test-only-hash", now0); !errors.Is(err, identity.ErrAdminRoleCombination) {
		t.Fatalf("legacy mixed authority: %v", err)
	}
	if _, err := st.UserBySubject(ctx, "test:legacy"); !errors.Is(err, identity.ErrUserNotFound) {
		t.Fatalf("rejected invitation created account: %v", err)
	}
	var redeemed bool
	if err := pool.QueryRow(ctx, `SELECT redeemed_at IS NOT NULL FROM identity_invites WHERE id='legacy'`).Scan(&redeemed); err != nil {
		t.Fatal(err)
	}
	if redeemed {
		t.Fatal("failed redemption committed")
	}
}
