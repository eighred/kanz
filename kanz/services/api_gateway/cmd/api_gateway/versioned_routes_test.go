package main

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/gatewaysig"
	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/pkg/auth"
)

// Start the normal composition root with a fresh issuer and only inert
// loopback upstreams. In particular, the child receives no broker address.
func TestGatewayVersionedRoutesAndDisabledWrites(t *testing.T) {
	const child = "KANZ_VERSIONED_ROUTE_TEST_CHILD"
	if os.Getenv(child) == "1" {
		if status := run(); status != 0 {
			t.Fatalf("gateway exited with status %d", status)
		}
		return
	}
	issuer := newIdentityStub(t)
	issuer.feedUp.Store(true)
	var forwards atomic.Int32
	upstream := func(method, pathSuffix string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			forwards.Add(1)
			p, ok := auth.PrincipalFromHeaders(r.Header)
			if !ok || p.Subject != "reader" || p.Tenant != "tenant-a" || !slices.Equal(p.Portfolios, []string{"fund-a"}) ||
				r.Method != method || !slices.Contains([]string{"/v1" + pathSuffix, "/v2" + pathSuffix}, r.URL.Path) {
				http.Error(w, "wrong forwarded scope or route", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(r.URL.Path)
		}))
		t.Cleanup(server.Close)
		return server
	}
	wealth := upstream(http.MethodGet, "/households/example")
	optimization := upstream(http.MethodPost, "/propose")
	key := rand.Text()
	lease, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lease.Addr().String()
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestGatewayVersionedRoutesAndDisabledWrites$")
	cmd.Env = []string{
		child + "=1", "API_GATEWAY_LISTEN=" + addr, "API_GATEWAY_RISK_ENGINE_ADDR=" + wealth.Listener.Addr().String(),
		"API_GATEWAY_WEALTH_ADDR=" + wealth.URL, "API_GATEWAY_OPTIMIZATION_ADDR=" + optimization.URL,
		"API_GATEWAY_OIDC_ISSUER=" + issuer.url, "API_GATEWAY_OIDC_AUDIENCE=kanz-api",
		"API_GATEWAY_OIDC_JWKS_URI=" + issuer.url + "/jwks.json", "API_GATEWAY_REVOCATIONS_URI=" + issuer.url + "/revocations",
		"API_GATEWAY_REQUIRED_ROLE=kanz-user", "API_GATEWAY_TRADE_ROLE=kanz-trader", "API_GATEWAY_APPROVE_ROLE=kanz-approver",
		"API_GATEWAY_SIGNING_SECRET=" + key,
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start isolated gateway: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(client.CloseIdleConnections)
	base := "http://" + addr
	ready := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		res, err := client.Get(base + "/readyz")
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatal("isolated gateway did not become ready")
	}
	token := func(subject string, roles ...string) string {
		t.Helper()
		value, _, err := issuer.signer.Mint(&identity.User{Subject: subject, Tenant: "tenant-a",
			Portfolios: []string{"fund-a"}, Roles: roles, Status: identity.StatusActive})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	reader := token("reader", "kanz-user")
	trader := token("trader", "kanz-user", "kanz-trader")
	approver := token("approver", "kanz-user", "kanz-approver")
	request := func(t *testing.T, method, path, body, bearer, version string, signed bool) *http.Response {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if version != "" {
			r.Header.Set("X-API-Version", version)
		}
		if signed {
			gatewaysig.SignRequest(r, []byte(key), []byte(body))
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	for _, tc := range []struct {
		name, method, path, body, bearer, upstreamPath string
		wantStatus                                     int
	}{
		{"v1_wealth", "GET", "/v1/households/example", "", reader, "/v1/households/example", 200},
		{"v2_wealth", "GET", "/v2/households/example", "", reader, "/v2/households/example", 200},
		{"v1_proposal", "POST", "/v1/model-portfolios/propose", "{}", reader, "/v1/propose", 200},
		{"v2_proposal", "POST", "/v2/model-portfolios/propose", "{}", reader, "/v2/propose", 200},
		{"anonymous_proposal", "POST", "/v2/model-portfolios/propose", "{}", "", "", 401},
		{"reader_write_denied", "POST", "/v1/orders", "{}", reader, "", 403},
		{"disabled_submit", "POST", "/v1/orders", `{"orderId":"example"}`, trader, "", 503},
		{"disabled_cancel", "POST", "/v1/orders/example/cancel", "{}", trader, "", 503},
		{"disabled_approve", "POST", "/v1/orders/example/approve", `{"digest":"example"}`, approver, "", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := forwards.Load()
			resp := request(t, tc.method, tc.path, tc.body, tc.bearer, "", true)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("HTTP %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			expected := before
			if tc.upstreamPath != "" {
				expected++
				var path string
				if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&path); err != nil || path != tc.upstreamPath {
					t.Fatalf("upstream path %q, want %q", path, tc.upstreamPath)
				}
			}
			if forwards.Load() != expected {
				t.Fatalf("upstream calls %d, want %d", forwards.Load(), expected)
			}
			if tc.wantStatus == http.StatusServiceUnavailable {
				var result struct{ Error string }
				if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result); err != nil ||
					result.Error != "order writes are disabled" {
					t.Fatal("missing disabled-write result")
				}
			}
		})
	}
	for _, version := range []string{"v1", "v2"} {
		for _, tc := range []struct {
			name, body, apiVersion string
			sign                   bool
			status                 int
		}{
			{"missing_signature", "{}", "", false, http.StatusUnauthorized},
			{"unsupported_version", "{}", "unknown", true, http.StatusNotAcceptable},
			{"oversized", strings.Repeat(" ", (1<<20)+1), "", true, http.StatusRequestEntityTooLarge},
		} {
			t.Run(version+"_"+tc.name, func(t *testing.T) {
				before := forwards.Load()
				resp := request(t, http.MethodPost, "/"+version+"/model-portfolios/propose", tc.body, reader, tc.apiVersion, tc.sign)
				if resp.StatusCode != tc.status || forwards.Load() != before {
					t.Fatalf("HTTP %d, want %d before any upstream call", resp.StatusCode, tc.status)
				}
			})
		}
	}
}
