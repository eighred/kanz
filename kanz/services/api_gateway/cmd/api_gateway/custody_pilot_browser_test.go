package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/services/api_gateway/internal/config"
	"github.com/eighred/kanz/services/api_gateway/internal/gateway"
	"github.com/eighred/kanz/services/api_gateway/internal/orders"
	"github.com/eighred/kanz/services/api_gateway/internal/proxy"
)

func custodyPilotEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() != "127.0.0.1" {
		return false
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	return err == nil && port > 0 && endpoint == "http://127.0.0.1:"+strconv.FormatUint(port, 10)
}

func TestCustodyPilotEndpoint(t *testing.T) {
	for _, host := range []string{"http://localhost:9900", "http://127.0.0.1:80/path", "http://127.0.0.1:1?x", "http://127.0.0.1:65536", "https://127.0.0.1:443", "http://127.0.0.1:0"} {
		if custodyPilotEndpoint(host) {
			t.Fatal("accepted unexpected test endpoint")
		}
	}
	if !custodyPilotEndpoint("http://127.0.0.1:8080") {
		t.Fatal("refused loopback fixture")
	}
}

// The accounting harness supplies the independently retained detection on stdin
// while its signed statement is still the active break. All identities are minted here.
func TestSignedCustodyOperatorBrowser(t *testing.T) { custodyBrowser(t, false) }
func TestSignedCustodyRerunBrowser(t *testing.T)    { custodyBrowser(t, true) }

func custodyBrowser(t *testing.T, rerun bool) {
	t.Helper()
	backendURL := os.Getenv("CUSTODY_PILOT_ACCOUNTING_URL")
	if backendURL == "" {
		t.Skip("requires a running synthetic accounting pilot")
	}
	if !custodyPilotEndpoint(backendURL) {
		t.Fatal("literal loopback pilot accounting required")
	}
	expected, err := io.ReadAll(io.LimitReader(os.Stdin, 16<<10))
	if err != nil || len(expected) >= 16<<10 || !json.Valid(expected) {
		t.Fatal("invalid expected pilot detection")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	id := newIdentityStub(t)
	id.feedUp.Store(true)
	authn, revocations := buildWired(t, id, &captureHandler{})
	if revocations.Refresh(ctx) != nil {
		t.Fatal("synthetic revocations unavailable")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{RequiredRole: baselineRole, FundRole: treasuryRole, CustodyRerunRole: "custody-pilot-rerun", CustodyReadRole: "custody-pilot-read"}
	hc := &http.Client{Transport: &http.Transport{}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer hc.CloseIdleConnections()
	be := proxy.NewMeshBackend(map[proxy.Service]string{proxy.ServiceAccounting: backendURL}, hc)
	var ready atomic.Bool
	ready.Store(true)
	router, err := buildRouter(cfg, gateway.New(nil, nil, nil, "", logger), orders.New(nil, "", halt.OpenGate(nil)), proxy.New(be, proxyRoles(cfg)), nil, testObservability(t), &ready, logger, nil, authn, nil)
	if err != nil {
		t.Fatal("gateway composition failed")
	}
	gw := httptest.NewServer(router)
	defer gw.Close()
	mint := func(name, tenant, portfolio string, fund bool) string {
		t.Helper()
		roles := []string{baselineRole}
		if fund && rerun {
			roles = append(roles, treasuryRole)
		} else if fund {
			roles = append(roles, cfg.CustodyReadRole)
		}
		subject := "custody-pilot-reader"
		if rerun {
			subject = "custody-pilot-rerun-" + name
			if name != "unfunded" {
				roles = append(roles, cfg.CustodyRerunRole)
			}
		}
		tok, _, err := id.signer.Mint(&identity.User{Subject: subject, Tenant: tenant, Portfolios: []string{portfolio}, Roles: roles, Status: identity.StatusActive})
		if err != nil {
			t.Fatal("fixture session unavailable")
		}
		return tok
	}
	const tenant = "synthetic-custody-pilot"
	claims := map[string]string{
		"authorized": mint("authorized", tenant, "PF1", true),
		"foreign":    mint("foreign", tenant+"-other", "PF1", true),
		"excluded":   mint("excluded", tenant, "PF2", true),
		"unfunded":   mint("unfunded", tenant, "PF1", rerun), // Fund-only for the rerun check.
	}
	b, err := json.Marshal(map[string]any{"rerun": rerun, "gateway": gw.URL, "issuer": id.url, "key": id.signer.JWKS().Keys[0], "claims": claims, "expected": json.RawMessage(expected)})
	if err != nil {
		t.Fatal("fixture input unavailable")
	}
	cmd := exec.CommandContext(ctx, os.Getenv("CUSTODY_PILOT_BFF_PROBE"), "-test.run=^TestCustodyPilotBFFBrowser$", "-test.count=1", "-test.v")
	cmd.Env = []string{"CUSTODY_PILOT_BFF_CHILD=1"}
	for _, key := range []string{"TEST_MFA_NODE", "TEST_MFA_PLAYWRIGHT", "TEST_MFA_STATIC_DIR", "CUSTODY_PILOT_BROWSER_SCRIPT", "PATH", "SystemRoot", "SystemDrive", "USERPROFILE", "LOCALAPPDATA", "TEMP", "TMP", "HOME", "PLAYWRIGHT_BROWSERS_PATH"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Stdin = bytes.NewReader(b)
	output, err := cmd.Output()
	if err != nil || !bytes.Contains(output, []byte("custody operator browser passed")) {
		// Emit only bounded stage labels; never forward a child's raw logs or credentials.
		if marker := regexp.MustCompile("custody-browser-stage=(?:input|launch|(?:authorized|foreign|excluded|unfunded)-(?:open|reload)-(?:response|payload|row|details))").Find(output); marker != nil {
			t.Fatal(string(marker))
		}
		for _, stage := range []string{"invalid fixture input", "loopback fixture required", "Vue distribution and actual BFF required", "fixture identity refused", "fixture identity scope refused", "fixture identity signature refused", "fixture identity claims refused", "synthetic BFF session failed", "cannot encode synthetic browser input", "Chromium probe could not start"} {
			if bytes.Contains(output, []byte(stage)) {
				t.Fatal(stage)
			}
		}
		t.Fatal("BFF child failed before browser verification")
	}
}
