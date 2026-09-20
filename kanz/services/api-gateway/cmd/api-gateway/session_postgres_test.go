package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Real PostgreSQL state, signed JWTs, HTTP JWKS/feed, and the gateway composition
// root together: a stale snapshot minted after commit must never regain admission.
func TestGatewaySessionFenceAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	boot, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	schema := fmt.Sprintf("gateway_session_%d", time.Now().UnixNano())
	name := pgx.Identifier{schema}.Sanitize()
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := boot.Exec(context.Background(), "DROP SCHEMA "+name+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "identity", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	for _, file := range files {
		sql, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity_users(subject,tenant_id,roles,credential_hash,status) VALUES
 ('admin','acme',ARRAY['kanz-identity-admin'],'synthetic','active'),('trader-a','acme',ARRAY['kanz-user'],'synthetic','active')`); err != nil {
		t.Fatal(err)
	}
	store := identity.NewPostgres(pool)
	st := newIdentityStub(t)
	st.feedUp.Store(true)
	authn, cache := buildWired(t, st, &captureHandler{})
	stale, err := store.UserBySubject(ctx, "trader-a")
	if err != nil {
		t.Fatal(err)
	}
	actor := identity.Administration{Subject: "admin", Tenant: "acme"}
	if err := store.SetStatus(ctx, actor, stale.Subject, identity.StatusDisabled, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	late, _, err := st.signer.Mint(stale)
	if err != nil {
		t.Fatal(err)
	}
	refresh := func() {
		t.Helper()
		entries, err := store.Revocations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		st.entries.Store(entries)
		if err := cache.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	refresh()
	if _, err := authn.Authenticate(late); !errors.Is(err, middleware.ErrUnauthenticated) {
		t.Fatalf("late login admitted: %v", err)
	}
	if err := store.SetStatus(ctx, actor, stale.Subject, identity.StatusActive, time.Now()); err != nil {
		t.Fatal(err)
	}
	current, err := store.UserBySubject(ctx, stale.Subject)
	if err != nil {
		t.Fatal(err)
	}
	fresh, _, err := st.signer.Mint(current)
	if err != nil {
		t.Fatal(err)
	}
	refresh()
	if _, err := authn.Authenticate(fresh); err != nil {
		t.Fatalf("fresh login: %v", err)
	}
	if _, err := authn.Authenticate(late); !errors.Is(err, middleware.ErrUnauthenticated) {
		t.Fatalf("enable resurrected session: %v", err)
	}
	// A newly started cache must reach the same answer from persisted generations.
	other, otherCache := buildWired(t, st, &captureHandler{})
	if err := otherCache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Authenticate(late); !errors.Is(err, middleware.ErrUnauthenticated) {
		t.Fatal("restart resurrected session")
	}
}
