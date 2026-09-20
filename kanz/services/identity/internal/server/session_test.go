package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/revocation"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/identity/internal/ratelimit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pausedSigner coordinates the actual HTTP login immediately before real signing;
// credential verification and all persistence still use production implementations.
type pausedSigner struct {
	signer  *identity.Signer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *pausedSigner) Mint(u *identity.User) (string, time.Time, error) {
	p.once.Do(func() { close(p.entered); <-p.release })
	return p.signer.Mint(u)
}

func TestHTTPLoginMintingAfterDisableCannotRegainGatewayAdmission(t *testing.T) {
	st, _ := sessionStorePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	now := time.Now()
	cred, err := identity.HashCredential("synthetic-session-regression")
	if err != nil {
		t.Fatal(err)
	}
	raw, hash, err := identity.NewInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	inv, err := identity.NewInvite("epoch", hash, "user:epoch", "acme", []string{"kanz-user"}, nil, "test:admin", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Redeem(ctx, raw, cred, now); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := identity.NewSigner(key, srv.URL, "kanz-api", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	paused := &pausedSigner{signer: signer, entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(paused.release) })
	s, err := New(st, paused, ratelimit.New(ratelimit.Options{}), func() any { return signer.JWKS() }, srv.URL, quiet())
	if err != nil {
		t.Fatal(err)
	}
	s.Routes(mux)
	reader, err := auth.NewOIDCAuthenticator(auth.OIDCConfig{Issuer: srv.URL, Audience: "kanz-api", JWKSURI: srv.URL + "/jwks.json"})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := revocation.New(revocation.Config{URL: srv.URL + "/revocations"})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		code  int
		token string
		err   error
	}
	login := func() result {
		req, e := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/login", strings.NewReader(`{"subject":"user:epoch","credential":"synthetic-session-regression"}`))
		if e != nil {
			return result{err: e}
		}
		resp, e := srv.Client().Do(req)
		if e != nil {
			return result{err: e}
		}
		defer func() { _ = resp.Body.Close() }()
		var body tokenResponse
		if resp.StatusCode == http.StatusOK {
			e = json.NewDecoder(resp.Body).Decode(&body)
		}
		return result{code: resp.StatusCode, token: body.Token, err: e}
	}
	done := make(chan result, 1)
	go func() { done <- login() }()
	select {
	case <-paused.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	actor := identity.Administration{Subject: "test:admin", Tenant: "acme"}
	if err := st.SetStatus(ctx, actor, "user:epoch", identity.StatusDisabled, now); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(paused.release) })
	late := <-done
	if late.err != nil || late.code != http.StatusOK {
		t.Fatalf("late login code=%d err=%v", late.code, late.err)
	}
	p, err := reader.Authenticate(ctx, late.token)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cache.Check(p.Subject, p.IssuedAt, p.SessionEpoch); !errors.Is(err, revocation.ErrRevoked) {
		t.Fatalf("late login admitted: %v", err)
	}
	if got := login(); got.err != nil || got.code != http.StatusUnauthorized {
		t.Fatalf("disabled login: %d %v", got.code, got.err)
	}
	if err := st.SetStatus(ctx, actor, "user:epoch", identity.StatusActive, now); err != nil {
		t.Fatal(err)
	}
	fresh := login()
	if fresh.err != nil || fresh.code != http.StatusOK {
		t.Fatalf("fresh login: %d %v", fresh.code, fresh.err)
	}
	q, err := reader.Authenticate(ctx, fresh.token)
	if err != nil {
		t.Fatal(err)
	}
	if q.SessionEpoch != 1 {
		t.Fatalf("lost generation: %d", q.SessionEpoch)
	}
	if err := cache.Check(q.Subject, q.IssuedAt, q.SessionEpoch); err != nil {
		t.Fatal(err)
	}
	if err := cache.Check(p.Subject, p.IssuedAt, p.SessionEpoch); !errors.Is(err, revocation.ErrRevoked) {
		t.Fatal("enable resurrected late login")
	}
}

func sessionStorePool(t *testing.T) (*identity.Postgres, *pgxpool.Pool) {
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
	cfg.ConnConfig.RuntimeParams["application_name"] = schema
	cfg.MaxConns = 4
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
	dir := filepath.Join("..", "..", "migrations")
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
	if _, err := pool.Exec(ctx, `INSERT INTO identity_users(subject, tenant_id, roles, credential_hash, status)
        VALUES ('test:admin', 'acme', ARRAY['kanz-identity-admin'], 'test-only-hash', 'active')`); err != nil {
		t.Fatal(err)
	}
	return identity.NewPostgres(pool), pool
}
