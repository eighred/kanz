package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
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
	identityserver "github.com/eighred/kanz/services/identity/internal/server"
)

// Optional local browser proof uses the same real database/SMTP fixture as the
// always-on integration test. Its script and compiled BFF are explicit inputs;
// no browser or external recipient is assumed by the normal Go suite.
func runRecoveryBrowser(t *testing.T, store *identity.Postgres, messages <-chan string) {
	t.Helper()
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := identity.NewSigner(key, "https://browser-identity.test", "kanz-api", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := identityserver.New(store, signer, ratelimit.New(ratelimit.Options{Burst: 100}), func() any { return signer.JWKS() }, "https://browser-identity.test", logger, identityserver.WithProvisioning(identityserver.Provisioning{Store: store, Verifier: signer, AdminRole: identity.AdminRole, Audit: auth.NewSlogRecorder(logger)}), identityserver.WithRecovery(store))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.Routes(mux)
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	inbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case message := <-messages:
			_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
		case <-r.Context().Done():
		case <-time.After(8 * time.Second):
			w.WriteHeader(408)
		}
	}))
	defer inbox.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	bff := exec.Command(os.Getenv("TEST_RECOVERY_BFF"))
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "WEB_BFF_") {
			bff.Env = append(bff.Env, entry)
		}
	}
	bff.Env = append(bff.Env, "WEB_BFF_LISTEN="+addr, "WEB_BFF_IDENTITY_URL="+upstream.URL, "WEB_BFF_GATEWAY_URL=http://127.0.0.1:1", "WEB_BFF_INSECURE_COOKIES=1", "WEB_BFF_STATIC_DIR="+os.Getenv("TEST_RECOVERY_STATIC_DIR"))
	if err = bff.Start(); err != nil {
		t.Fatal("BFF launch failed")
	}
	defer func() { _ = bff.Process.Kill(); _ = bff.Wait() }()
	base := "http://" + addr
	client := &http.Client{Timeout: time.Second}
	ready := false
	for range 50 {
		resp, e := client.Get(base + "/readyz")
		if e == nil {
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
	input, _ := json.Marshal(map[string]string{"base": base, "inbox": inbox.URL, "subject": "worker-user", "credential": "synthetic-worker-password", "replacement": "synthetic-browser-new-password"})
	browserCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(browserCtx, os.Getenv("TEST_RECOVERY_NODE"), os.Getenv("TEST_RECOVERY_BROWSER_SCRIPT"))
	cmd.Stderr = os.Stderr
	cmd.Stdin = bytes.NewReader(input)
	// Browser errors may contain request data. Print only the exit outcome.
	if err = cmd.Run(); err != nil {
		t.Fatal("real browser recovery verification failed")
	}
}
