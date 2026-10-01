package identity_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAccessMutationIsVersionedAuditedAndRevoked(t *testing.T) {
	st, pool := newStorePool(t)
	audit := auditPool(t, pool, "acme")
	ctx := context.Background()
	raw := invite(t, st, "access-invite", "alice")
	u, err := st.Redeem(ctx, raw, identity.Hash("test-only-hash"), now0)
	if err != nil {
		t.Fatal(err)
	}
	page, err := st.UsersFor(ctx, testAdmin, "")
	if err != nil || len(page) != 2 {
		t.Fatalf("directory: %v %d", err, len(page))
	}
	if page[0].CreatedBy != "user:operator" {
		t.Fatal("creator provenance missing")
	}
	a, err := st.SetAccess(ctx, testAdmin, u.Subject, 0, []string{"kanz-user"}, []string{"pf-2"}, now0.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if a.Revision != 1 || a.SessionEpoch != 1 || a.Portfolios[0] != "pf-2" {
		t.Fatalf("wrong state: %+v", a)
	}
	if _, err = st.SetAccess(ctx, testAdmin, u.Subject, 0, []string{"kanz-trader"}, nil, now0); !errors.Is(err, identity.ErrAccessConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	fresh, err := st.UserBySubject(ctx, u.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if (identity.Administration{Subject: u.Subject, Tenant: u.Tenant, IssuedAt: now0.Add(time.Hour)}).Current(fresh) {
		t.Fatal("late-minted stale snapshot was admitted")
	}
	rev, err := st.Revocations(ctx)
	if err != nil || len(rev) != 1 || rev[0].SessionEpoch != 1 {
		t.Fatalf("revocation: %v %+v", err, rev)
	}
	var payload string
	if err = audit.QueryRow(ctx, `SELECT decision::text FROM identity_access_audit`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"principal.subject", "test:admin", "account.before", "account.after", "pf-1", "pf-2", "identity.account.access"} {
		if !strings.Contains(payload, part) {
			t.Fatalf("audit missing %s", part)
		}
	}
	if strings.Contains(payload, "test-only-hash") {
		t.Fatal("credential in audit")
	}
	for _, q := range []string{`UPDATE identity_access_audit SET subject='tampered'`, `DELETE FROM identity_access_audit`, `TRUNCATE identity_access_audit`} {
		if _, err = audit.Exec(ctx, q); err == nil {
			t.Fatal("audit mutation accepted")
		}
	}
}

func TestAccessAuditFailureRollsBackAuthorityAndStatus(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	raw := invite(t, st, "rollback-invite", "alice")
	if _, err := st.Redeem(ctx, raw, identity.Hash("test-only-hash"), now0); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION refuse_access_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test audit outage'; END $$;
 CREATE TRIGGER refuse_access_audit BEFORE INSERT ON identity_access_audit FOR EACH STATEMENT EXECUTE FUNCTION refuse_access_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccess(ctx, testAdmin, "alice", 0, []string{"kanz-user"}, nil, now0); err == nil {
		t.Fatal("access committed without audit")
	}
	if err := st.SetStatus(ctx, testAdmin, "alice", identity.StatusDisabled, now0); err == nil {
		t.Fatal("status committed without audit")
	}
	u, err := st.UserBySubject(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Active() || u.SessionEpoch != 0 || u.TokensInvalidBefore != nil || u.Roles[0] != "kanz-trader" {
		t.Fatal("failed transaction changed authority")
	}
}

func TestConcurrentAccessEditsHaveOneWinner(t *testing.T) {
	st, pool := newStorePool(t)
	audit := auditPool(t, pool, "acme")
	ctx := context.Background()
	raw := invite(t, st, "race-invite", "alice")
	if _, err := st.Redeem(ctx, raw, identity.Hash("test-only-hash"), now0); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	start := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			<-start
			_, err := st.SetAccess(ctx, testAdmin, "alice", 0, []string{"kanz-user"}, nil, now0)
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
		} else if !errors.Is(err, identity.ErrAccessConflict) {
			t.Fatal(err)
		}
	}
	var count int
	if err := audit.QueryRow(ctx, `SELECT count(*) FROM identity_access_audit`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if wins != 1 || count != 1 {
		t.Fatalf("wins=%d audit=%d", wins, count)
	}
}

func auditPool(t *testing.T, pool *pgxpool.Pool, tenant string) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["app.tenant_id"] = tenant
	scoped, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scoped.Close)
	return scoped
}

func TestAccessAuditRLSRefusesUnscopedAndForeignReaders(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	// Keeping the admin role still advances the epoch and produces evidence.
	if _, err := st.SetAccess(ctx, testAdmin, testAdmin.Subject, 0, []string{identity.AdminRole}, nil, now0); err != nil {
		t.Fatal(err)
	}
	var unscopedCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_access_audit`).Scan(&unscopedCount); err == nil {
		t.Fatal("missing tenant scope silently returned an audit history")
	}
	for _, tc := range []struct {
		pool *pgxpool.Pool
		want int
	}{{auditPool(t, pool, "foreign"), 0}, {auditPool(t, pool, "acme"), 1}} {
		var count int
		if err := tc.pool.QueryRow(ctx, `SELECT count(*) FROM identity_access_audit`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != tc.want {
			t.Fatalf("RLS count=%d want=%d", count, tc.want)
		}
	}
	foreign := auditPool(t, pool, "foreign")
	if _, err := foreign.Exec(ctx, `INSERT INTO identity_access_audit(tenant_id,subject,occurred_at,decision) VALUES('acme','forged',now(),'{}')`); err == nil {
		t.Fatal("foreign audit insertion accepted")
	}
}

func TestAccessPreservesLastAdminAndTenantIsolation(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	if _, err := st.SetAccess(ctx, testAdmin, testAdmin.Subject, 0, []string{"kanz-user"}, nil, now0); !errors.Is(err, identity.ErrLastAdmin) {
		t.Fatalf("last admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity_users(subject,tenant_id,roles,credential_hash) VALUES('other','other-tenant',ARRAY['kanz-user'],'test-only-hash')`); err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{"other", "missing"} {
		if _, err := st.SetAccess(ctx, testAdmin, subject, 0, []string{"kanz-user"}, nil, now0); !errors.Is(err, identity.ErrUserNotFound) {
			t.Fatalf("tenant leak: %v", err)
		}
	}
	page, err := st.UsersFor(ctx, testAdmin, "")
	if err != nil || len(page) != 1 {
		t.Fatalf("tenant directory: %v %d", err, len(page))
	}
	bad := testAdmin
	bad.SessionEpoch = 1
	if _, err = st.UsersFor(ctx, bad, ""); !errors.Is(err, identity.ErrAdminAuthority) {
		t.Fatalf("stale directory access: %v", err)
	}
	if _, err = st.SetAccess(ctx, bad, testAdmin.Subject, 0, []string{identity.AdminRole}, nil, now0); !errors.Is(err, identity.ErrAdminAuthority) {
		t.Fatalf("stale edit: %v", err)
	}
}

func TestCompetingAdminDemotionsCannotRemoveBoth(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO identity_users(subject,tenant_id,roles,credential_hash) VALUES('second-admin','acme',ARRAY['kanz-identity-admin'],'test-only-hash')`); err != nil {
		t.Fatal(err)
	}
	second := identity.Administration{Subject: "second-admin", Tenant: "acme"}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, pair := range []struct {
		actor  identity.Administration
		target string
	}{{testAdmin, second.Subject}, {second, testAdmin.Subject}} {
		wg.Go(func() {
			<-start
			_, err := st.SetAccess(ctx, pair.actor, pair.target, 0, []string{"kanz-user"}, nil, now0)
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
		} else if !errors.Is(err, identity.ErrAdminAuthority) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("demotions=%d", wins)
	}
}

func TestAccessValidation(t *testing.T) {
	for _, roles := range [][]string{nil, {""}, {"kanz-user", "kanz-user"}, {" kanz-user"}, {identity.AdminRole, "kanz-trader"}} {
		if identity.ValidateAccess(roles, nil) == nil {
			t.Fatalf("accepted %v", roles)
		}
	}
	if identity.ValidateAccess([]string{"kanz-user"}, []string{""}) == nil {
		t.Fatal("accepted empty portfolio")
	}
}

func TestUserDirectoryPagesDoNotRepeatOrCrossTenants(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO identity_users(subject,tenant_id,roles,credential_hash)
 SELECT 'page-' || lpad(n::text,3,'0'), CASE WHEN n%2=0 THEN 'acme' ELSE 'foreign' END, ARRAY['kanz-user'], 'test-only-hash' FROM generate_series(1,110) n`)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	after := ""
	for {
		page, err := st.UsersFor(ctx, testAdmin, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > identity.UserPageSize {
			t.Fatal("unbounded page")
		}
		for _, user := range page {
			if user.Tenant != "acme" || seen[user.Subject] {
				t.Fatal("directory repeated or leaked a user")
			}
			seen[user.Subject] = true
		}
		if len(page) < identity.UserPageSize {
			break
		}
		after = page[len(page)-1].Subject
	}
	if len(seen) != 56 {
		t.Fatalf("directory lost accounts: %d", len(seen))
	}
}

func TestAccessQueueFailureRollsBackAuthorityAndStatus(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	raw := invite(t, st, "rollback-invite", "alice")
	if _, err := st.Redeem(ctx, raw, identity.Hash("test-only-hash"), now0); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION refuse_access_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test audit outage'; END $$;
 CREATE TRIGGER refuse_access_audit BEFORE INSERT ON identity_access_audit_pending FOR EACH STATEMENT EXECUTE FUNCTION refuse_access_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccess(ctx, testAdmin, "alice", 0, []string{"kanz-user"}, nil, now0); err == nil {
		t.Fatal("access committed without audit")
	}
	if err := st.SetStatus(ctx, testAdmin, "alice", identity.StatusDisabled, now0); err == nil {
		t.Fatal("status committed without audit")
	}
	u, err := st.UserBySubject(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Active() || u.SessionEpoch != 0 || u.TokensInvalidBefore != nil || u.Roles[0] != "kanz-trader" {
		t.Fatal("failed transaction changed authority")
	}
}
