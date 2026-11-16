package session

import (
	"bytes"
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_URL required for real session authority proof")
	}
	ctx := context.Background()
	boot, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer boot.Close()
	var bypass bool
	if e = boot.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&bypass); e != nil || bypass {
		t.Fatal("NOSUPERUSER NOBYPASSRLS role required")
	}
	schema := fmt.Sprintf("bff_test_%d", time.Now().UnixNano())
	if _, e = boot.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); e != nil {
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 4
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		pool.Close()
		b, e := pgxpool.New(ctx, dsn)
		if e != nil {
			t.Error(e)
			return
		}
		defer b.Close()
		if _, e = b.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); e != nil {
			t.Error(e)
		}
	})
	files, e := filepath.Glob("../../migrations/*.sql")
	if e != nil || len(files) == 0 {
		t.Fatal("migrations missing")
	}
	for _, f := range files {
		raw, e := os.ReadFile(f)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = pool.Exec(ctx, string(raw)); e != nil {
			t.Fatal(e)
		}
	}
	return pool
}
func TestSharedPostgresAuthority(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	key := bytes.Repeat([]byte{7}, 32)
	limits := Limits{Sessions: 4, Pending: 2, PerSubject: 2}
	a, e := NewPostgres(ctx, pool, key, time.Hour, limits)
	if e != nil {
		t.Fatal(e)
	}
	b, e := NewPostgres(ctx, pool, key, time.Hour, limits)
	if e != nil {
		t.Fatal(e)
	}
	authorityContract(t, a, b)
	if _, e := NewPostgres(ctx, pool, bytes.Repeat([]byte{8}, 32), time.Hour, limits); e != ErrInvalid {
		t.Fatal("different replica key accepted", e)
	}
	if _, e := NewPostgres(ctx, pool, key, time.Minute, limits); e != ErrInvalid {
		t.Fatal("different replica TTL accepted", e)
	}
	if _, e := NewPostgres(ctx, pool, key, time.Hour, Limits{5, 2, 2}); e != ErrInvalid {
		t.Fatal("different replica quota accepted", e)
	}
}
func TestBoundedMemoryAuthority(t *testing.T) {
	m, e := NewBounded(time.Hour, Limits{4, 2, 2})
	if e != nil {
		t.Fatal(e)
	}
	authorityContract(t, m, m)
}
func authorityContract(t *testing.T, a, b *Manager) {
	t.Helper()
	ctx := context.Background()
	s := Session{Subject: "alice", Tenant: "fund-a", AccessToken: "synthetic-bearer"}
	first, e := a.Create(ctx, s)
	if e != nil {
		t.Fatal(e)
	}
	second, e := b.Create(ctx, s)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.Create(ctx, s); e != ErrCapacity {
		t.Fatal("per-owner bound", e)
	}
	if _, ok, e := b.Get(ctx, first); e != nil || !ok {
		t.Fatal("replica lost session", e)
	}
	rows, e := b.List(ctx, first)
	if e != nil || len(rows) != 2 {
		t.Fatal("inventory", e, rows)
	}
	for _, r := range rows {
		if r.ID == first || r.ID == second {
			t.Fatal("bearer cookie exposed")
		}
		if _, ok, e := b.Get(ctx, r.ID); e != nil || ok {
			t.Fatal("inventory id is a bearer")
		}
	}
	other := s
	other.Tenant = "fund-b"
	foreign, e := a.Create(ctx, other)
	if e != nil {
		t.Fatal(e)
	}
	if e = b.Revoke(ctx, first, digest(foreign)); e != ErrNotFound {
		t.Fatal("cross-tenant revoke", e)
	}
	other = s
	other.Authority = "oidc:https://other.test"
	provider, e := a.Create(ctx, other)
	if e != nil {
		t.Fatal(e)
	}
	if e = b.Revoke(ctx, first, digest(provider)); e != ErrNotFound {
		t.Fatal("cross-provider revoke", e)
	}
	other = s
	other.Subject = "bob"
	if _, e = a.Create(ctx, other); e != ErrCapacity {
		t.Fatal("global capacity", e)
	}
	// Replacement at full capacity succeeds atomically and old cookie is dead.
	rotated, e := a.Replace(ctx, first, s, true)
	if e != nil {
		t.Fatal(e)
	}
	if _, ok, e := b.Get(ctx, first); e != nil || ok {
		t.Fatal("old session survived", e)
	}
	// A failed cross-owner replacement cannot destroy the previous session.
	if _, e = b.Replace(ctx, rotated, other, true); e != ErrMissing {
		t.Fatal("owner changed during step-up", e)
	}
	if e = b.Revoke(ctx, second, digest(rotated)); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Replace(ctx, rotated, s, true); e != ErrMissing {
		t.Fatal("revoked session resurrected", e)
	}
	// Exactly one concurrent callback consumes the verifier across replicas.
	if e = a.PutPending(ctx, "pending-1", "synthetic-pkce"); e != nil {
		t.Fatal(e)
	}
	if e = b.PutPending(ctx, "pending-2", "v"); e != nil {
		t.Fatal(e)
	}
	if e = a.PutPending(ctx, "pending-3", "v"); e != ErrCapacity {
		t.Fatal("pending bound", e)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := a
			if i%2 == 0 {
				m = b
			}
			v, ok, e := m.TakePending(ctx, "pending-1")
			if e != nil {
				t.Error(e)
			}
			if ok {
				if v != "synthetic-pkce" {
					t.Error("corrupt verifier")
				}
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("callback replay", wins.Load())
	}
	for _, id := range []string{second, foreign, provider} {
		if e = a.Delete(ctx, id); e != nil {
			t.Fatal(e)
		}
	}
	// Competing strict rotations cannot both mint an active successor.
	old, e := a.Create(ctx, s)
	if e != nil {
		t.Fatal(e)
	}
	wins.Store(0)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := a
			if i%2 == 0 {
				m = b
			}
			_, e := m.Replace(ctx, old, s, true)
			if e == nil {
				wins.Add(1)
			} else if e != ErrMissing {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("multiple rotation successors", wins.Load())
	}
}
func TestPostgresEncryptionRLSAuditAndOutage(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	key := bytes.Repeat([]byte{9}, 32)
	m, e := NewPostgres(ctx, p, key, time.Hour, DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	s := Session{Subject: "alice", Tenant: "fund-a", AccessToken: "synthetic-bearer", RefreshToken: "synthetic-refresh"}
	id, e := m.Create(ctx, s)
	if e != nil {
		t.Fatal(e)
	}
	tx, e := p.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if e = scope(ctx, tx, Session{Tenant: "fund-b", Subject: "alice", Authority: "native"}); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM bff_sessions`).Scan(&n); e != nil || n != 0 {
		t.Fatal("RLS leaked rows", n, e)
	}
	if e = scope(ctx, tx, Session{Tenant: "fund-a", Subject: "bob", Authority: "native"}); e != nil {
		t.Fatal(e)
	}
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM bff_sessions`).Scan(&n); e != nil || n != 0 {
		t.Fatal("subject isolation", n, e)
	}
	if e = scope(ctx, tx, Session{Tenant: "fund-a", Subject: "alice", Authority: "native"}); e != nil {
		t.Fatal(e)
	}
	var payload []byte
	var hash string
	if e = tx.QueryRow(ctx, `SELECT hash,payload FROM bff_sessions`).Scan(&hash, &payload); e != nil {
		t.Fatal(e)
	}
	if hash == id || bytes.Contains(payload, []byte(s.AccessToken)) || bytes.Contains(payload, []byte(s.RefreshToken)) {
		t.Fatal("plaintext credential stored")
	}
	var auditText string
	if e = tx.QueryRow(ctx, `SELECT decision::text FROM bff_session_audit LIMIT 1`).Scan(&auditText); e != nil {
		t.Fatal(e)
	}
	if bytes.Contains([]byte(auditText), []byte(id)) || bytes.Contains([]byte(auditText), []byte(s.AccessToken)) {
		t.Fatal("audit exposed bearer")
	}
	if _, e = tx.Exec(ctx, `DELETE FROM bff_session_audit`); e == nil {
		t.Fatal("audit mutable")
	}
	_ = tx.Rollback(ctx)
	// Corrupting expiry metadata cannot lengthen an authenticated encrypted session.
	if _, e = p.Exec(ctx, `UPDATE bff_session_slots SET expires_at=expires_at+interval '1 hour' WHERE hash=$1`, digest(id)); e != nil {
		t.Fatal(e)
	}
	if _, _, e = m.Get(ctx, id); e != ErrUnavailable {
		t.Fatal("unauthenticated expiry accepted", e)
	}
	// Expiry collection cascades through forced RLS even without an owner scope.
	if _, e = p.Exec(ctx, `UPDATE bff_session_slots SET expires_at=clock_timestamp()-interval '1 second'`); e != nil {
		t.Fatal(e)
	}
	if e = m.Sweep(ctx); e != nil {
		t.Fatal("expiry cascade", e)
	}
	if _, ok, e := m.Get(ctx, id); e != nil || ok {
		t.Fatal("expired session persisted", e)
	}
	p.Close()
	if e = m.Ping(ctx); e != ErrUnavailable {
		t.Fatal("outage ready", e)
	}
	if _, _, e = m.Get(ctx, id); e != ErrUnavailable {
		t.Fatal("outage failed open", e)
	}
	if _, e = m.Create(ctx, s); e != ErrUnavailable {
		t.Fatal("outage local fallback", e)
	}
}

func TestPostgresConcurrentCapacityAndAuditRollback(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	m, e := NewPostgres(ctx, p, bytes.Repeat([]byte{10}, 32), time.Hour, Limits{4, 2, 2})
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	var wins atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, e := m.Create(ctx, Session{Subject: fmt.Sprintf("user-%d", i), Tenant: "fund", AccessToken: "test"})
			if e == nil {
				wins.Add(1)
			} else if e != ErrCapacity {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 4 {
		t.Fatal("global quota race", wins.Load())
	}
	if _, e = p.Exec(ctx, `UPDATE bff_session_slots SET expires_at=clock_timestamp()-interval '1 second'`); e != nil {
		t.Fatal(e)
	}
	if e = m.Sweep(ctx); e != nil {
		t.Fatal(e)
	}
	s := Session{Subject: "alice", Tenant: "fund", AccessToken: "test"}
	old, e := m.Create(ctx, s)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.Exec(ctx, `CREATE FUNCTION refuse_session_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic audit outage'; END $$; CREATE TRIGGER refuse_session_audit BEFORE INSERT ON bff_session_audit FOR EACH ROW EXECUTE FUNCTION refuse_session_audit()`); e != nil {
		t.Fatal(e)
	}
	if _, e = m.Replace(ctx, old, s, true); e != ErrUnavailable {
		t.Fatal("unaudited replacement committed", e)
	}
	if _, ok, e := m.Get(ctx, old); e != nil || !ok {
		t.Fatal("failed replacement destroyed old session", e)
	}
	if e = m.Delete(ctx, old); e != ErrUnavailable {
		t.Fatal("unaudited logout committed", e)
	}
	if _, ok, e := m.Get(ctx, old); e != nil || !ok {
		t.Fatal("failed logout destroyed session", e)
	}
	if _, e = p.Exec(ctx, `DROP TRIGGER refuse_session_audit ON bff_session_audit`); e != nil {
		t.Fatal(e)
	}
	if e = m.Delete(ctx, old); e != nil {
		t.Fatal(e)
	}
}

func TestPostgresRefusesUnprotectedSchema(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	if _, e := p.Exec(ctx, `ALTER TABLE bff_sessions NO FORCE ROW LEVEL SECURITY`); e != nil {
		t.Fatal(e)
	}
	if _, e := NewPostgres(ctx, p, bytes.Repeat([]byte{11}, 32), time.Hour, DefaultLimits()); e != ErrUnavailable {
		t.Fatal("unprotected owner schema accepted", e)
	}
}
