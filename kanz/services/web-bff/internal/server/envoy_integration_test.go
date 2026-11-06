package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/clientip"
	"github.com/eighred/kanz/internal/gatewaysig"
	"github.com/eighred/kanz/internal/requestbody"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/web-bff/internal/edgeconfig"
	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
	"github.com/eighred/kanz/services/web-bff/internal/session"
)

// These fixtures run only on an ephemeral hosted Linux runner. They contact a
// real pinned Envoy and the actual BFF; identity/gateway endpoints are synthetic.
// This does not expose the currently buffered gateway's broker SSE route.
type envoyProcess struct {
	name, base, admin string
	closed            bool
}

func dockerOutput(ctx context.Context, args ...string) ([]byte, error) {
	// Never print arbitrary daemon/process output (or a request/access log).
	return exec.CommandContext(ctx, "docker", args...).Output()
}

func freeEdgePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("allocate fixture listener")
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}

func (p *envoyProcess) close() error {
	if p.closed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	remaining := func() ([]byte, error) {
		return dockerOutput(ctx, "ps", "-a", "--filter", "name=^/"+p.name+"$", "--format", "{{.ID}}")
	}
	before, err := remaining()
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(before)) > 0 {
		if _, err := dockerOutput(ctx, "rm", "--force", p.name); err != nil {
			return err
		}
	}
	after, err := remaining()
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(after)) != 0 {
		return fmt.Errorf("fixture container remains")
	}
	p.closed = true
	return nil
}

func startEnvoy(t *testing.T, bffURL string, rate int, shortenIdle, removeStreamOverride bool) *envoyProcess {
	t.Helper()
	u, err := url.Parse(bffURL)
	if err != nil {
		t.Fatal("fixture upstream URL")
	}
	var bffPort int
	if _, err := fmt.Sscanf(u.Port(), "%d", &bffPort); err != nil {
		t.Fatal("fixture upstream port")
	}
	o := edgeconfig.Default()
	o.BFFPort, o.ListenPort, o.AdminPort = bffPort, freeEdgePort(t), freeEdgePort(t)
	o.AdminLoopback, o.RequestsPerSecond = true, rate
	b, err := edgeconfig.Build(o)
	if err != nil {
		t.Fatal("build fixture configuration")
	}
	// Shortened timer control discriminates quiet-stream behavior without adding
	// five minutes to CI. Production's normal-route timer remains 300 seconds.
	if shortenIdle {
		b = bytes.Replace(b, []byte(`"stream_idle_timeout": "300s"`), []byte(`"stream_idle_timeout": "0.1s"`), 1)
	}
	if removeStreamOverride {
		var config map[string]any
		if json.Unmarshal(b, &config) != nil {
			t.Fatal("parse stream control")
		}
		resources := config["static_resources"].(map[string]any)
		listener := resources["listeners"].([]any)[0].(map[string]any)
		chain := listener["filter_chains"].([]any)[0].(map[string]any)
		filter := chain["filters"].([]any)[0].(map[string]any)
		hcm := filter["typed_config"].(map[string]any)
		vhost := hcm["route_config"].(map[string]any)["virtual_hosts"].([]any)[0].(map[string]any)
		route := vhost["routes"].([]any)[0].(map[string]any)["route"].(map[string]any)
		delete(route, "idle_timeout")
		b, err = json.Marshal(config)
		if err != nil {
			t.Fatal("serialize stream control")
		}
	}
	path := filepath.Join(t.TempDir(), "envoy.json")
	if os.WriteFile(path, b, 0644) != nil { // Public configuration; container reads one RO file.
		t.Fatal("write public proxy configuration")
	}
	p := &envoyProcess{name: "kanz-envoy-proof-" + strings.ToLower(rand.Text()),
		base: fmt.Sprintf("http://127.0.0.1:%d", o.ListenPort), admin: fmt.Sprintf("http://127.0.0.1:%d", o.AdminPort)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = dockerOutput(ctx, "run", "--detach", "--name", p.name, "--network", "host",
		"--platform", "linux/amd64", "--user", "65532:65532", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--cpus", "1", "--memory", "512m", "--pids-limit", "64",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=16m", "--mount", "type=bind,src="+path+",dst=/etc/envoy/edge.json,readonly",
		edgeconfig.Image(), "-c", "/etc/envoy/edge.json", "--concurrency", "1", "--disable-hot-restart", "--log-level", "error")
	// Even a failed startup may leave a stopped container.
	t.Cleanup(func() {
		if err := p.close(); err != nil {
			t.Error("disposable proxy cleanup failed")
		}
	})
	if err != nil {
		t.Fatal("pinned Envoy startup failed")
	}
	client := &http.Client{Timeout: time.Second}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		resp, err := client.Get(p.admin + "/ready")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return p
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("proxy never became ready")
	return nil
}

func TestEnvoyEdgeActualBFF(t *testing.T) {
	if os.Getenv("TEST_ENVOY_IMAGE") == "" {
		t.Skip("requires the dedicated actual Envoy qualification image")
	}
	if os.Getenv("TEST_ENVOY_IMAGE") != edgeconfig.Image() || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" ||
		os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("DOCKER_HOST") != "" ||
		(os.Getenv("DOCKER_CONTEXT") != "" && os.Getenv("DOCKER_CONTEXT") != "default") {
		t.Fatal("actual proxy fixture requires the pinned image and hosted local Linux Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	imageID, err := dockerOutput(ctx, "image", "inspect", "--format", "{{.Id}}", edgeconfig.Image())
	if err != nil || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(strings.TrimSpace(string(imageID))) {
		t.Fatal("qualification image was not loaded")
	}
	t.Log("ENVOY_IMAGE_OBSERVED=" + strings.TrimSpace(string(imageID)))

	const heldToken = "synthetic-edge-session-token"
	const signingKey = "synthetic-edge-request-signing-key"
	type observed struct {
		path, rawPath, query, host, cf string
		valid, clean                   bool
		body                           []byte
	}
	seen := make(chan observed, 64)
	var retryCalls atomic.Int32
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, requestbody.MaxBytes+1))
		valid := err == nil && r.Header.Get("Authorization") == "Bearer "+heldToken && r.Header.Get("Cookie") == "" &&
			gatewaysig.Verify([]byte(signingKey), r.Method, r.URL.Path, body, r.Header.Get("X-Signature"))
		clean := true
		for _, name := range []string{auth.HeaderPrincipalSubject, auth.HeaderPrincipalTenant, auth.HeaderPrincipalRoles, auth.HeaderPrincipalPortfolios, "X-Forwarded-Client-Cert", "X-Envoy-Retry-On", "X-Envoy-Max-Retries"} {
			clean = clean && r.Header.Get(name) == ""
		}
		seen <- observed{path: r.URL.Path, rawPath: r.URL.RawPath, query: r.URL.RawQuery, host: r.Host, cf: r.Header.Get("CF-Connecting-IP"), valid: valid, clean: clean, body: body}
		if !valid {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v1/retry-proof" {
			retryCalls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-time.After(time.Second):
				_, _ = io.WriteString(w, "data: synthetic-delta\n\n")
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer gw.Close()
	var observedIP atomic.Value
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedIP.Store(r.Header.Get("X-Kanz-Client-IP"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"`+heldToken+`","expires_at":"2099-01-01T00:00:00Z","subject":"user:edge-proof","tenant":"edge-proof"}`)
	}))
	defer idp.Close()
	resolver, err := clientip.NewResolver("CF-Connecting-IP", []string{"127.0.0.1"})
	if err != nil {
		t.Fatal("loopback identity resolver")
	}
	ready := &Readiness{}
	ready.Set(true)
	srv, err := New(ready, Options{Identity: identityclient.New(idp.URL, "X-Kanz-Client-IP", time.Second), ClientIP: resolver,
		Sessions: session.NewManager(time.Hour), GatewayURL: gw.URL, SigningSecret: signingKey, SecureCookies: true})
	if err != nil {
		t.Fatal("actual BFF fixture startup")
	}
	defer srv.Close()
	bff := httptest.NewServer(srv)
	defer bff.Close()
	id, err := srv.sessions.Create(context.Background(), session.Session{AccessToken: heldToken, Subject: "user:edge-proof", Tenant: "edge-proof", Expiry: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal("synthetic session setup")
	}
	p := startEnvoy(t, bff.URL, 200, false, false)
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(t *testing.T, base, method, path, body string, authorized bool, headers map[string]string) *http.Response {
		t.Helper()
		r, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal("fixture request")
		}
		if authorized {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
		}
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal("proxy request failed")
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	readObserved := func(t *testing.T) observed {
		t.Helper()
		select {
		case x := <-seen:
			return x
		case <-time.After(time.Second):
			t.Fatal("gateway did not observe the authenticated request")
			return observed{}
		}
	}
	t.Run("anonymous", func(t *testing.T) {
		if request(t, p.base, "GET", "/api/v1/proof", "", false,
			map[string]string{auth.HeaderPrincipalRoles: "admin", auth.HeaderPrincipalTenant: "forged", "Authorization": "Bearer forged-synthetic-token"}).StatusCode != http.StatusUnauthorized {
			t.Fatal("anonymous request was admitted")
		}
		select {
		case <-seen:
			t.Fatal("anonymous request reached the gateway")
		default:
		}
	})
	t.Run("session_gateway_signing", func(t *testing.T) {
		if request(t, p.base, "GET", "/api/v1/proof", "", true, map[string]string{"Authorization": "Bearer forged-synthetic-token"}).StatusCode != http.StatusNoContent || !readObserved(t).valid {
			t.Fatal("server-held session/signature boundary changed")
		}
	})
	t.Run("origin_csrf", func(t *testing.T) {
		if request(t, p.base, "POST", "/api/v1/proof", "{}", true, map[string]string{"Origin": "https://untrusted.invalid", "Sec-Fetch-Site": "cross-site"}).StatusCode != http.StatusForbidden {
			t.Fatal("cross-origin unsafe method was admitted")
		}
	})
	t.Run("encoded_identifiers", func(t *testing.T) {
		for _, value := range []string{"BTC/USD", "X?as_of=other", "X#fragment", "X%2FY"} {
			if request(t, p.base, "GET", "/api/v1/proof/"+url.PathEscape(value)+"?probe=%2B%20%26", "", true, nil).StatusCode != http.StatusNoContent {
				t.Fatal("encoded identifier request failed")
			}
			x := readObserved(t)
			if x.path != "/v1/proof/"+value || x.query != "probe=%2B%20%26" || !x.valid {
				t.Fatal("encoded identifier/query/signature changed")
			}
		}
	})
	t.Run("principal_sanitization", func(t *testing.T) {
		h := map[string]string{auth.HeaderPrincipalSubject: "forged", auth.HeaderPrincipalTenant: "forged", auth.HeaderPrincipalRoles: "admin", auth.HeaderPrincipalPortfolios: `["forged"]`, "X-Forwarded-Client-Cert": "forged", "CF-Connecting-IP": "198.51.100.7"}
		if request(t, p.base, "GET", "/api/v1/proof", "", true, h).StatusCode != http.StatusNoContent {
			t.Fatal("principal sanitation request failed")
		}
		x := readObserved(t)
		if !x.clean || x.cf != "198.51.100.7" {
			t.Fatal("untrusted identity survived or original edge address changed")
		}
	})
	t.Run("request_body", func(t *testing.T) {
		body := `{"synthetic":"exact + %2F bytes"}`
		if request(t, p.base, "POST", "/api/v1/proof", body, true, map[string]string{"Origin": p.base}).StatusCode != http.StatusNoContent {
			t.Fatal("same-origin body was refused")
		}
		x := readObserved(t)
		if !x.valid || string(x.body) != body {
			t.Fatal("body/signature changed")
		}
	})
	t.Run("cookies", func(t *testing.T) {
		resp := request(t, p.base, "POST", "/auth/login", `{"subject":"user:edge-proof","credential":"synthetic"}`, false,
			map[string]string{"Origin": p.base, "Content-Type": "application/json", "CF-Connecting-IP": "198.51.100.7"})
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if err != nil || resp.StatusCode != http.StatusOK || bytes.Contains(body, []byte(heldToken)) || observedIP.Load() != "198.51.100.7" {
			t.Fatal("login/token isolation or original client attribution changed")
		}
		cookies := resp.Cookies()
		if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
			t.Fatal("session cookie policy changed")
		}
	})
	t.Run("admin_surface", func(t *testing.T) {
		for _, path := range []string{"/config_dump", "/quitquitquit", "/runtime_modify"} {
			if request(t, p.admin, "GET", path, "", false, nil).StatusCode != http.StatusForbidden {
				t.Fatal("unexpected control surface")
			}
		}
	})
	t.Run("no_retry", func(t *testing.T) {
		if request(t, p.base, "POST", "/api/v1/retry-proof", "{}", true, map[string]string{"Origin": p.base, "X-Envoy-Retry-On": "5xx", "X-Envoy-Max-Retries": "5"}).StatusCode != http.StatusServiceUnavailable {
			t.Fatal("upstream failure status changed")
		}
		if !readObserved(t).clean || retryCalls.Load() != 1 {
			t.Fatal("browser control headers caused a retry")
		}
	})
	for _, control := range []bool{false, true} {
		name := "quiet_stream"
		if control {
			name = "quiet_stream_negative_control"
		}
		t.Run(name, func(t *testing.T) {
			streamProxy := startEnvoy(t, bff.URL, 200, true, control)
			resp := request(t, streamProxy.base, "GET", "/api/v1/broker/accounts/proof/stream", "", true, nil)
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatal("controlled stream was not established")
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
			if !readObserved(t).valid {
				t.Fatal("controlled stream was not authenticated at the gateway")
			}
			if !control && (err != nil || string(body) != "data: synthetic-delta\n\n") {
				t.Fatal("quiet controlled stream was cut before its delta")
			}
			if control && bytes.Contains(body, []byte("synthetic-delta")) {
				t.Fatal("missing stream-idle override failed to discriminate")
			}
			if streamProxy.close() != nil {
				t.Fatal("stream proxy cleanup")
			}
		})
	}
	t.Run("rate_limit", func(t *testing.T) {
		limited := startEnvoy(t, bff.URL, 1, false, false)
		allowed, denied := 0, 0
		for i := 0; i < 8; i++ {
			status := request(t, limited.base, "GET", "/healthz", "", false, nil).StatusCode
			switch status {
			case http.StatusOK:
				allowed++
			case http.StatusTooManyRequests:
				denied++
			default:
				t.Fatal("unexpected rate-limit outcome")
			}
		}
		if allowed == 0 || denied == 0 || limited.close() != nil {
			t.Fatal("actual rate enforcement or cleanup was not observed")
		}
	})
	t.Run("cleanup", func(t *testing.T) {
		if p.close() != nil {
			t.Fatal("main proxy cleanup")
		}
	})
}
