package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/revocation"
)

func TestSessionEpochFencesLoginAcrossBlockedDisableAndReenable(t *testing.T) {
	st, pool := newStorePool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw := invite(t, st, "epoch-invite", "user:epoch")
	if _, err := st.Redeem(ctx, raw, "test-only-hash", now0); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('identity-administration:acme',0))`); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- st.SetStatus(ctx, testAdmin, "user:epoch", identity.StatusDisabled, now0) }()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=current_setting('application_name') AND wait_event_type='Lock' AND wait_event='advisory')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("disable escaped lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Login reads while disable is blocked, after its caller captured the timestamp.
	stale, err := st.UserBySubject(ctx, "user:epoch")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	mintedAt := now0.Add(time.Hour)
	signer, err := identity.NewSigner(key, "issuer", "aud", time.Hour, identity.WithClock(func() time.Time { return mintedAt }))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := signer.Mint(stale)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := signer.Verify(token, mintedAt)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		entries, e := st.Revocations(r.Context())
		if e != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		_ = json.NewEncoder(w).Encode(revocation.Feed{Kind: revocation.FeedKind, Entries: entries})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cache, err := revocation.New(revocation.Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		if err := cache.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		if err := cache.Check(claims.Subject, claims.IssuedAt, claims.SessionEpoch); !errors.Is(err, revocation.ErrRevoked) {
			t.Fatalf("late stale login admitted: %v", err)
		}
	}
	check()
	if err := st.SetStatus(ctx, testAdmin, stale.Subject, identity.StatusActive, now0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	check()
	fresh, err := st.UserBySubject(ctx, stale.Subject)
	if err != nil {
		t.Fatal(err)
	}
	// Clock rollback cannot revive the old generation or prevent a fresh login.
	mintedAt = now0.Add(-time.Minute)
	token, _, err = signer.Mint(fresh)
	if err != nil {
		t.Fatal(err)
	}
	current, err := signer.Verify(token, mintedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Check(current.Subject, current.IssuedAt, current.SessionEpoch); err != nil {
		t.Fatalf("fresh generation refused: %v", err)
	}
	if fresh.SessionEpoch != 1 {
		t.Fatalf("generation=%d", fresh.SessionEpoch)
	}
	if err := st.SetStatus(ctx, testAdmin, stale.Subject, identity.StatusDisabled, now0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	check()
	if err := cache.Check(current.Subject, current.IssuedAt, current.SessionEpoch); !errors.Is(err, revocation.ErrRevoked) {
		t.Fatalf("second disable failed: %v", err)
	}
}

func TestSessionMigrationBackfillsWithoutResetAndOverflowFailsClosed(t *testing.T) {
	st, pool := newStorePool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO identity_users(subject,tenant_id,roles,credential_hash,status,tokens_invalid_before,session_epoch) VALUES
 ('legacy-disabled','acme',ARRAY['kanz-user'],'synthetic','disabled',now(),0),
 ('legacy-enabled','acme',ARRAY['kanz-user'],'synthetic','active',now(),0),
 ('advanced','acme',ARRAY['kanz-user'],'synthetic','active',now(),9223372036854775807)`); err != nil {
		t.Fatal(err)
	}
	sql, err := os.ReadFile(filepath.Join("..", "..", "services", "identity", "migrations", "0003_session_epoch.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	for _, subject := range []string{"legacy-disabled", "legacy-enabled"} {
		u, err := st.UserBySubject(ctx, subject)
		if err != nil || u.SessionEpoch != 1 {
			t.Fatalf("backfill %s: %v %v", subject, u, err)
		}
	}
	if err := st.SetStatus(ctx, testAdmin, "advanced", identity.StatusDisabled, now0); err == nil {
		t.Fatal("overflow committed")
	}
	u, err := st.UserBySubject(ctx, "advanced")
	if err != nil || !u.Active() || u.SessionEpoch != 9223372036854775807 {
		t.Fatalf("overflow changed authority: %v %v", u, err)
	}
}
