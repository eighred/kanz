package identity_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/eighred/kanz/internal/identity"
)

func TestAccessRevisionExhaustionHasNoAuthorityOrAuditEffect(t *testing.T) {
	for _, field := range []string{"access_revision", "session_epoch"} {
		t.Run(field, func(t *testing.T) {
			store, pool := newStorePool(t)
			ctx := context.Background()
			if _, err := store.Redeem(ctx, invite(t, store, "wire-target", "alice"), identity.Hash("test-only-hash"), now0); err != nil {
				t.Fatal(err)
			}
			// Only these two named fixture columns may be selected by this test.
			if _, err := pool.Exec(ctx, `UPDATE identity_users SET `+field+`=$1 WHERE subject='alice'`, int64(math.MaxInt64-1)); err != nil {
				t.Fatal(err)
			}
			before, err := store.UsersFor(ctx, testAdmin, "")
			if err != nil {
				t.Fatal(err)
			}
			var current *identity.Access
			for _, row := range before {
				if row.Subject == "alice" {
					current = row
				}
			}
			if current == nil {
				t.Fatal("fixture target missing")
			}
			first, err := store.SetAccess(ctx, testAdmin, "alice", current.Revision, []string{"kanz-user"}, []string{"wire-first"}, now0)
			if err != nil {
				t.Fatal("last representable increment refused", err)
			}
			if field == "access_revision" && first.Revision != math.MaxInt64 || field == "session_epoch" && first.SessionEpoch != math.MaxInt64 {
				t.Fatal("boundary increment was not exact")
			}
			if _, err := store.SetAccess(ctx, testAdmin, "alice", first.Revision, []string{"kanz-user", "kanz-trader"}, []string{"wire-forbidden"}, now0); !errors.Is(err, identity.ErrAccessConflict) {
				t.Fatal("exhausted counter did not refuse")
			}
			var revision, epoch int64
			var portfolios, roles []string
			if err := pool.QueryRow(ctx, `SELECT access_revision,session_epoch,roles,portfolios FROM identity_users WHERE subject='alice'`).Scan(&revision, &epoch, &roles, &portfolios); err != nil {
				t.Fatal(err)
			}
			if revision != first.Revision || epoch != first.SessionEpoch || len(roles) != 1 || roles[0] != "kanz-user" || len(portfolios) != 1 || portfolios[0] != "wire-first" {
				t.Fatal("exhausted counter changed authority")
			}
			var count int
			if err := auditPool(t, pool, "acme").QueryRow(ctx, `SELECT count(*) FROM identity_access_audit WHERE subject='alice'`).Scan(&count); err != nil || count != 1 {
				t.Fatal("exhausted counter wrote audit evidence")
			}
		})
	}
}

func TestInvitationRevisionExhaustionDoesNotRevokeReissueOrAudit(t *testing.T) {
	store, pool := newStorePool(t)
	ctx := context.Background()
	invite(t, store, "wire-invite", "alice")
	if _, err := pool.Exec(ctx, `UPDATE identity_invites SET revision=$1 WHERE id='wire-invite'`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RevokeInvite(ctx, testAdmin, "wire-invite", math.MaxInt64, now0); !errors.Is(err, identity.ErrInviteConflict) {
		t.Fatal("exhausted revoke did not refuse")
	}
	_, hash, err := identity.NewInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReissueInvite(ctx, testAdmin, "wire-invite", math.MaxInt64, "wire-replacement", hash, identity.InviteDomainPolicy{}, now0, 0); !errors.Is(err, identity.ErrInviteConflict) {
		t.Fatal("exhausted reissue did not refuse")
	}
	list, err := store.InvitesFor(ctx, "acme")
	if err != nil || len(list) != 1 || list[0].Revision != math.MaxInt64 || list[0].RevokedAt != nil || list[0].ReissuedAs != nil {
		t.Fatal("exhausted invitation changed or created a replacement")
	}
	var count int
	if err := auditPool(t, pool, "acme").QueryRow(ctx, `SELECT count(*) FROM identity_access_audit WHERE subject='alice'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("exhausted invitation wrote audit evidence")
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_invites SET revision=$1 WHERE id='wire-invite'`, int64(math.MaxInt64-1)); err != nil {
		t.Fatal(err)
	}
	last, err := store.RevokeInvite(ctx, testAdmin, "wire-invite", math.MaxInt64-1, now0)
	if err != nil || last.Revision != math.MaxInt64 {
		t.Fatal("last invitation increment was not exact")
	}
	if _, err := store.RevokeInvite(ctx, testAdmin, "wire-invite", math.MaxInt64, now0); err != nil {
		t.Fatal("already-revoked idempotent response changed", err)
	}
}
