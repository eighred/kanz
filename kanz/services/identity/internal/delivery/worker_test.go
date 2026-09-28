package delivery

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkerDeliversDurablePostgresChallengeThroughRealSMTP(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	boot, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	var super bool
	if err = boot.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&super); err != nil || super {
		t.Fatal("NOSUPERUSER required", err)
	}
	schema := pgx.Identifier{fmt.Sprintf("mail_worker_%d", time.Now().UnixNano())}.Sanitize()
	if _, err = boot.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = boot.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) }()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatal("missing migrations", err)
	}
	for _, file := range files {
		b, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = pool.Exec(ctx, string(b)); e != nil {
			t.Fatal(e)
		}
	}
	store := identity.NewPostgres(pool)
	now := time.Now()
	raw, hash, err := identity.NewInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	inv, err := identity.NewInvite("worker-test", hash, "worker-user", "acme", []string{"kanz-user"}, nil, "test:admin", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	cred, err := identity.HashCredential("synthetic-worker-password")
	if err != nil {
		t.Fatal(err)
	}
	u, err := store.Redeem(ctx, raw, cred, now)
	if err != nil {
		t.Fatal(err)
	}
	actor := identity.Administration{Subject: u.Subject, Tenant: u.Tenant, IssuedAt: now}
	browser := os.Getenv("TEST_RECOVERY_BROWSER_SCRIPT") != ""
	if !browser {
		if err = store.EnrollMailbox(ctx, actor, cred, "person@example.test", now); err != nil {
			t.Fatal(err)
		}
	}
	client, messages := smtpService(t, "starttls", false)
	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- Run(workerCtx, pool, store, client, "https://kanz.test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("worker did not stop")
		}
	}()
	if browser {
		runRecoveryBrowser(t, store, messages)
		return
	}
	var message string
	select {
	case message = <-messages:
	case <-ctx.Done():
		t.Fatal("SMTP delivery timed out")
	}
	parts := strings.SplitN(message, "#token=", 2)
	if len(parts) != 2 {
		t.Fatal("missing verification link")
	}
	proof := strings.Fields(parts[1])[0]
	if err = store.ConsumeChallenge(ctx, proof, "verify", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	status, err := store.MailboxStatus(ctx, actor, time.Now())
	if err != nil || status.Address != "person@example.test" {
		t.Fatal("mailbox proof not committed", err)
	}
	if err = store.RequestRecovery(ctx, u.Subject, time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case message = <-messages:
	case <-ctx.Done():
		t.Fatal("notification did not wake recovery delivery")
	}
	if !strings.Contains(message, "/recover#token=") {
		t.Fatal("wrong purpose delivered")
	}
	proof = strings.Fields(strings.SplitN(message, "#token=", 2)[1])[0]
	replacement, err := identity.HashCredential("synthetic-worker-replacement")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ConsumeChallenge(ctx, proof, "recover", replacement, time.Now()); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.UserBySubject(ctx, u.Subject)
	if err != nil || fresh.SessionEpoch != 1 {
		t.Fatal("mail recovery did not revoke sessions", err)
	}
}
