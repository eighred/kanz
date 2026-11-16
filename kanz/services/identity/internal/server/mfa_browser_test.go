package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/identity/internal/ratelimit"
	"github.com/go-webauthn/webauthn/webauthn"
)

func TestMFABrowserEnrollmentLoginStepUpAndFactorRemoval(t *testing.T) {
	script := os.Getenv("TEST_MFA_BROWSER_SCRIPT")
	if script == "" {
		t.Skip("optional real Chromium proof requires TEST_MFA_BROWSER_SCRIPT")
	}
	store, pool := sessionStorePool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	hash, err := identity.HashCredential("synthetic-mfa-browser-password")
	if err != nil {
		t.Fatal(err)
	}
	raw, digest, err := identity.NewInviteToken()
	if err != nil {
		t.Fatal(err)
	}
	inv, err := identity.NewInvite("mfa-browser", digest, "mfa-browser-user", "acme", []string{identity.AdminRole}, nil, "test:admin", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Redeem(ctx, raw, hash, now); err != nil {
		t.Fatal(err)
	}
	// The real HTTP/browser flow must retain adjacent revisions beyond 2^53,
	// without changing the administrator's own session epoch or MFA authority.
	for _, target := range []struct {
		subject         string
		revision, epoch int64
	}{{"wire-a", 1 << 53, 1 << 53}, {"wire-b", 1<<53 + 1, 0}, {"wire-max", math.MaxInt64, 0}} {
		if _, err := pool.Exec(ctx, `INSERT INTO identity_users(subject,tenant_id,roles,portfolios,credential_hash,status,access_revision,session_epoch)
		 VALUES($1,'acme',ARRAY['kanz-user'],ARRAY['wire-original'],$2,'active',$3,$4)`, target.subject, string(hash), target.revision, target.epoch); err != nil {
			t.Fatal("seed isolated integer target")
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity_users(subject,tenant_id,roles,credential_hash,status) VALUES('wire-foreign','other',ARRAY['kanz-user'],$1,'active')`, string(hash)); err != nil {
		t.Fatal("seed isolated foreign account")
	}
	for _, target := range []struct {
		id       string
		revision int64
	}{{"wire-revoke-a", 1 << 53}, {"wire-revoke-b", 1<<53 + 1}, {"wire-reissue", 1<<53 + 1}, {"wire-max-invite", math.MaxInt64}} {
		_, tokenHash, err := identity.NewInviteToken()
		if err != nil {
			t.Fatal("create isolated invitation token")
		}
		invitation, err := identity.NewInvite(target.id, tokenHash, target.id, "acme", []string{"kanz-user"}, nil, "mfa-browser-user", now, time.Hour)
		if err != nil {
			t.Fatal("construct isolated invitation")
		}
		if err := store.CreateInvite(ctx, invitation); err != nil {
			t.Fatal("store isolated invitation")
		}
		if _, err := pool.Exec(ctx, `UPDATE identity_invites SET revision=$2 WHERE id=$1`, target.id, target.revision); err != nil {
			t.Fatal("seed isolated invitation revision")
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	_ = listener.Close()
	base := "http://localhost:" + port
	wa, err := webauthn.New(&webauthn.Config{RPID: "localhost", RPOrigins: []string{base}, RPDisplayName: "Kanz test"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := identity.NewSigner(key, "https://mfa-browser.test", "kanz-api", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(store, signer, ratelimit.New(ratelimit.Options{Burst: 200}), func() any { return signer.JWKS() }, "https://mfa-browser.test", quiet(), WithProvisioning(Provisioning{Store: store, Verifier: signer, AdminRole: identity.AdminRole, Audit: auth.NewSlogRecorder(quiet())}), WithMFA(MFAConfig{Store: store, WebAuthn: wa, RequirePrivileged: true}), WithRecovery(store))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Routes(mux)
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	bff := exec.Command(os.Getenv("TEST_MFA_BFF"))
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "WEB_BFF_") {
			bff.Env = append(bff.Env, entry)
		}
	}
	bff.Env = append(bff.Env, "WEB_BFF_LISTEN="+addr, "WEB_BFF_IDENTITY_URL="+upstream.URL, "WEB_BFF_GATEWAY_URL=http://127.0.0.1:1", "WEB_BFF_INSECURE_COOKIES=1", "WEB_BFF_STATIC_DIR="+os.Getenv("TEST_MFA_STATIC_DIR"))
	if err = bff.Start(); err != nil {
		t.Fatal("BFF launch failed")
	}
	defer func() { _ = bff.Process.Kill(); _ = bff.Wait() }()
	client := &http.Client{Timeout: time.Second}
	ready := false
	for range 50 {
		resp, e := client.Get(base + "/readyz")
		if e == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("BFF not ready")
	}
	input, _ := json.Marshal(map[string]string{"base": base, "subject": "mfa-browser-user", "credential": "synthetic-mfa-browser-password"})
	run, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(run, os.Getenv("TEST_MFA_NODE"), script)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = os.Stderr
	if err = cmd.Run(); err != nil {
		t.Fatal("real browser MFA verification failed")
	}
	fresh, err := store.UserBySubject(ctx, "mfa-browser-user")
	if err != nil || !fresh.MFA.Required || fresh.SessionEpoch != 3 {
		t.Fatal("browser did not commit two enrollments and one removal", err)
	}
	for _, target := range []struct {
		subject         string
		revision, epoch int64
	}{
		{"wire-a", 1<<53 + 1, 1<<53 + 1}, {"wire-b", 1<<53 + 2, 1}, {"wire-max", math.MaxInt64, 0},
	} {
		var revision, epoch int64
		var portfolios []string
		if err := pool.QueryRow(ctx, `SELECT access_revision,session_epoch,portfolios FROM identity_users WHERE subject=$1`, target.subject).Scan(&revision, &epoch, &portfolios); err != nil {
			t.Fatal(err)
		}
		expectedPortfolio := "wire-edited"
		if target.subject == "wire-max" {
			expectedPortfolio = "wire-original"
		}
		if revision != target.revision || epoch != target.epoch || len(portfolios) != 1 || portfolios[0] != expectedPortfolio {
			t.Fatal("browser integer mutation did not preserve exact state or exhaustion rollback")
		}
	}
	for _, target := range []struct {
		id       string
		revision int64
		reissued bool
	}{
		{"wire-revoke-a", 1<<53 + 1, false}, {"wire-revoke-b", 1<<53 + 2, false}, {"wire-reissue", 1<<53 + 2, true},
	} {
		var revision int64
		var revoked, reissued bool
		if err := pool.QueryRow(ctx, `SELECT revision,revoked_at IS NOT NULL,reissued_as IS NOT NULL FROM identity_invites WHERE id=$1`, target.id).Scan(&revision, &revoked, &reissued); err != nil {
			t.Fatal("read isolated invitation outcome")
		}
		if revision != target.revision || !revoked || reissued != target.reissued {
			t.Fatal("browser invitation revision/lineage was not exact")
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id','acme',true)`); err != nil {
		t.Fatal(err)
	}
	var exhaustedAudits int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity_access_audit WHERE subject IN ('wire-max','wire-max-invite')`).Scan(&exhaustedAudits); err != nil || exhaustedAudits != 0 {
		t.Fatal("exhausted mutation produced audit effects")
	}
	var revision int64
	var revoked, replacement bool
	if err := pool.QueryRow(ctx, `SELECT revision,revoked_at IS NOT NULL,reissued_as IS NOT NULL FROM identity_invites WHERE id='wire-max-invite'`).Scan(&revision, &revoked, &replacement); err != nil || revision != math.MaxInt64 || revoked || replacement {
		t.Fatal("exhausted invitation mutation changed state")
	}
	t.Log("integer HTTP/BFF/browser proof: adjacent signed revisions, exact mutation requests, stale/malformed/exhausted refusal and unchanged authority")
}
