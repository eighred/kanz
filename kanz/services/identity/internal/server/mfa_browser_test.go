package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
	store, _ := sessionStorePool(t)
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
}
