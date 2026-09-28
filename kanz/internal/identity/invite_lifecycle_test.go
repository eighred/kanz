package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
)

func TestInviteReissueInvalidatesOriginalAndPreservesAuthority(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	old := invite(t, st, "original", "alice")
	raw, hash, err := identity.NewInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	i, err := st.ReissueInvite(ctx, testAdmin, "original", 0, "replacement", hash, identity.InviteDomainPolicy{}, now0.Add(time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if i.Subject != "alice" || i.Tenant != "acme" || i.Roles[0] != "kanz-trader" || i.Portfolios[0] != "pf-1" || i.ExpiresAt != now0.Add(2*time.Hour) {
		t.Fatal("reissue changed authority or expiry")
	}
	if _, err = st.Redeem(ctx, old, identity.Hash("test-only-hash"), now0.Add(time.Hour)); !errors.Is(err, identity.ErrInviteNotFound) {
		t.Fatal("old token survived reissue", err)
	}
	u, err := st.Redeem(ctx, raw, identity.Hash("test-only-hash"), now0.Add(time.Hour))
	if err != nil || u.Subject != "alice" {
		t.Fatal("fresh token failed", err)
	}
	if _, err = st.Redeem(ctx, raw, identity.Hash("test-only-hash"), now0.Add(time.Hour)); !errors.Is(err, identity.ErrInviteNotFound) {
		t.Fatal("reissued token was reusable", err)
	}
	list, err := st.InvitesFor(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]identity.InvitationSummary{}
	for _, v := range list {
		states[v.ID] = v.Summary(now0.Add(time.Hour))
	}
	if states["original"].State != "revoked" || states["replacement"].State != "accepted" || *states["original"].ReissuedAs != "replacement" {
		t.Fatal("incorrect lifecycle projection")
	}
	var audit string
	if err = auditPool(t, pool, "acme").QueryRow(ctx, `SELECT decision::text FROM identity_access_audit`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	projection, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{old, raw, hash, identity.InviteTokenHash(old)} {
		if strings.Contains(audit, secret) || strings.Contains(string(projection), secret) {
			t.Fatal("invitation token material leaked")
		}
	}
	for _, part := range []string{"identity.invitation.reissue", "original", "replacement", "principal.subject", "principal.tenant", "kanz-trader", "pf-1"} {
		if !strings.Contains(audit, part) {
			t.Fatal("missing canonical audit evidence", part)
		}
	}
}

func TestInvitationMutationAndRedemptionHaveOneWinner(t *testing.T) {
	for _, action := range []string{"revoke", "reissue"} {
		t.Run(action, func(t *testing.T) {
			st, pool := newStorePool(t)
			ctx := context.Background()
			audit := auditPool(t, pool, "acme")
			for n := range 12 {
				id := fmt.Sprintf("invite-%d", n)
				subject := fmt.Sprintf("person-%d", n)
				old := invite(t, st, id, subject)
				raw, hash, err := identity.NewInviteToken()
				if err != nil {
					t.Fatal(err)
				}
				start := make(chan struct{})
				redeemed := make(chan error, 1)
				mutated := make(chan error, 1)
				go func() { <-start; _, err := st.Redeem(ctx, old, identity.Hash("test-only-hash"), now0); redeemed <- err }()
				go func() {
					<-start
					var err error
					if action == "revoke" {
						_, err = st.RevokeInvite(ctx, testAdmin, id, 0, now0)
					} else {
						_, err = st.ReissueInvite(ctx, testAdmin, id, 0, id+"-new", hash, identity.InviteDomainPolicy{}, now0, time.Hour)
					}
					mutated <- err
				}()
				close(start)
				red, mutation := <-redeemed, <-mutated
				if (red == nil) == (mutation == nil) {
					t.Fatalf("expected one winner: redeem=%v mutation=%v", red, mutation)
				}
				var count int
				if err = audit.QueryRow(ctx, `SELECT count(*) FROM identity_access_audit WHERE subject=$1`, subject).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if mutation == nil {
					if !errors.Is(red, identity.ErrInviteNotFound) || count != 1 {
						t.Fatal("mutation winner did not fence redemption/audit", red, count)
					}
					if action == "reissue" {
						if _, err = st.Redeem(ctx, raw, identity.Hash("test-only-hash"), now0); err != nil {
							t.Fatal(err)
						}
					}
				} else if !errors.Is(mutation, identity.ErrInviteConflict) || count != 0 {
					t.Fatal("accepted invitation was mutated", mutation, count)
				}
			}
		})
	}
}

func TestCompetingReissuesConsumeOneReviewedRevision(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	invite(t, st, "source", "alice")
	results := make(chan error, 8)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for n := range 8 {
		_, hash, err := identity.NewInviteToken()
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() {
			<-start
			_, err := st.ReissueInvite(ctx, testAdmin, "source", 0, fmt.Sprintf("replacement-%d", n), hash, identity.InviteDomainPolicy{}, now0, time.Hour)
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, identity.ErrInviteConflict) {
			t.Fatal(err)
		}
	}
	var count int
	if err := auditPool(t, pool, "acme").QueryRow(ctx, `SELECT count(*) FROM identity_access_audit`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	list, err := st.InvitesFor(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if wins != 1 || count != 1 || len(list) != 2 {
		t.Fatalf("wins=%d audits=%d invites=%d", wins, count, len(list))
	}
}

func TestInviteLifecycleRefusesStaleAuthorityAndRollsBackAuditFailure(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	old := invite(t, st, "source", "alice")
	_, hash, err := identity.NewInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.RevokeInvite(ctx, identity.Administration{Subject: testAdmin.Subject, Tenant: testAdmin.Tenant, SessionEpoch: 99}, "source", 0, now0); !errors.Is(err, identity.ErrAdminAuthority) {
		t.Fatal("stale admin admitted", err)
	}
	if _, err = st.RevokeInvite(ctx, testAdmin, "missing", 0, now0); !errors.Is(err, identity.ErrInviteNotFound) {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE identity_invites SET tenant_id='foreign' WHERE id='source'`); err != nil {
		t.Fatal(err)
	}
	if _, err = st.RevokeInvite(ctx, testAdmin, "source", 0, now0); !errors.Is(err, identity.ErrInviteNotFound) {
		t.Fatal("foreign invitation visible", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE identity_invites SET tenant_id='acme' WHERE id='source'`); err != nil {
		t.Fatal(err)
	}
	policy, err := identity.ParseInviteDomainPolicy("eighred.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ReissueInvite(ctx, testAdmin, "source", 0, "blocked", hash, policy, now0, time.Hour); !errors.Is(err, identity.ErrInvitePolicy) {
		t.Fatal("domain policy bypassed", err)
	}
	if _, err = pool.Exec(ctx, `CREATE FUNCTION refuse_invitation_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit outage'; END $$; CREATE TRIGGER refuse_invitation_audit BEFORE INSERT ON identity_access_audit FOR EACH STATEMENT EXECUTE FUNCTION refuse_invitation_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err = st.RevokeInvite(ctx, testAdmin, "source", 0, now0); err == nil {
		t.Fatal("revoke committed without audit")
	}
	if _, err = st.ReissueInvite(ctx, testAdmin, "source", 0, "replacement", hash, identity.InviteDomainPolicy{}, now0, time.Hour); err == nil {
		t.Fatal("reissue committed without audit")
	}
	list, err := st.InvitesFor(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Revision != 0 || list[0].RevokedAt != nil || list[0].ReissuedAs != nil {
		t.Fatal("audit failure changed invitation")
	}
	if _, err = st.Redeem(ctx, old, identity.Hash("test-only-hash"), now0); err != nil {
		t.Fatal("failed mutation burned invitation", err)
	}
}

func TestInviteLifecycleMigrationPreservesExistingOffersAndAllowsExpiredReissue(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	old := invite(t, st, "legacy", "alice")
	// Restore the pre-0005 schema, leaving the actual legacy invitation in place.
	if _, err := pool.Exec(ctx, `DROP INDEX identity_invites_one_unrevoked_per_subject; ALTER TABLE identity_invites DROP COLUMN reissued_as; ALTER TABLE identity_invites DROP COLUMN revision; ALTER TABLE identity_invites DROP COLUMN revoked_by; ALTER TABLE identity_invites DROP COLUMN revoked_at; CREATE UNIQUE INDEX identity_invites_one_live_per_subject ON identity_invites(subject) WHERE redeemed_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	ddl, err := os.ReadFile(filepath.Join("..", "..", "services", "identity", "migrations", "0005_invitation_lifecycle.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := st.InvitesFor(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Revision != 0 || list[0].Summary(now0).State != "pending" || list[0].Summary(now0.Add(4*24*time.Hour)).State != "expired" {
		t.Fatal("migration changed existing offer")
	}
	var hash string
	if err = pool.QueryRow(ctx, `SELECT token_hash FROM identity_invites WHERE id='legacy'`).Scan(&hash); err != nil || hash != identity.InviteTokenHash(old) {
		t.Fatal("migration changed token")
	}
	revoked, err := st.RevokeInvite(ctx, testAdmin, "legacy", 0, now0.Add(4*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.RevokeInvite(ctx, testAdmin, "legacy", revoked.Revision, now0.Add(4*24*time.Hour)); err != nil {
		t.Fatal("reviewed repeat revoke failed", err)
	}
	_, hash, err = identity.NewInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ReissueInvite(ctx, testAdmin, "legacy", 0, "stale", hash, identity.InviteDomainPolicy{}, now0.Add(4*24*time.Hour), time.Hour); !errors.Is(err, identity.ErrInviteConflict) {
		t.Fatal("stale review accepted", err)
	}
	if _, err = st.ReissueInvite(ctx, testAdmin, "legacy", revoked.Revision, "fresh", hash, identity.InviteDomainPolicy{}, now0.Add(4*24*time.Hour), time.Hour); err != nil {
		t.Fatal("expired revoked offer could not be reissued", err)
	}
}
