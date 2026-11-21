package agent_boundary_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const readerRole = "agent.reader"

// Subprocesses receive only fixture configuration, never the runner's ambient
// credentials, database URLs, proxy settings or workload socket.
func fixtureProcess(t *testing.T, binary string, values map[string]string, readyURL string) {
	t.Helper()
	if !filepath.IsAbs(binary) {
		t.Fatal("fixture binary must be an absolute path")
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("fixture binary unavailable: %s", filepath.Base(binary))
	}
	scratch := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	cmd := exec.CommandContext(ctx, binary)
	cmd.Dir = scratch
	cmd.Env = []string{"HOME=" + scratch, "TMPDIR=" + scratch, "TZ=UTC", "LANG=C.UTF-8", "GOMAXPROCS=2", "GOMEMLIMIT=256MiB"}
	for key, value := range values {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	logs := &boundedLog{}
	cmd.Stdout, cmd.Stderr = logs, logs
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start %s: %v", filepath.Base(binary), err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cancel()
			_ = cmd.Process.Kill()
			<-done
		}
		cancel()
	})
	client := proofClient()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			done <- err
			t.Fatalf("%s exited before readiness: %v; startup messages=%s", filepath.Base(binary), err, logs.messages())
		default:
		}
		response, err := client.Get(readyURL)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s did not become ready; startup messages=%s", filepath.Base(binary), logs.messages())
}

type boundedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *boundedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if remaining := (1 << 20) - b.b.Len(); remaining > 0 {
		_, _ = b.b.Write(p[:min(remaining, len(p))])
	}
	return n, nil
}

// Only source-owned startup message names are projected on failure. Raw logs
// can contain principal/resource details and are not evidence artifacts.
func (b *boundedLog) messages() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var messages []string
	for _, line := range bytes.Split(b.b.Bytes(), []byte{'\n'}) {
		var row struct {
			Message string `json:"msg"`
		}
		if json.Unmarshal(line, &row) == nil && row.Message != "" {
			messages = append(messages, row.Message)
		}
	}
	if len(messages) > 8 {
		messages = messages[len(messages)-8:]
	}
	return strings.Join(messages, "; ")
}

func loopbackAddress(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func proofClient() *http.Client {
	return &http.Client{
		Timeout:       3 * time.Second,
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type fixtureIssuer struct {
	key    *rsa.PrivateKey
	server *httptest.Server
}

func newFixtureIssuer(t *testing.T) *fixtureIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &fixtureIssuer{key: key}
	issuer.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "agent-boundary-fixture",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}}})
		case "/revocations":
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "kanz.revocations.v2", "as_of": time.Now().Unix(), "entries": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(issuer.server.Close)
	return issuer
}

func (i *fixtureIssuer) token(t *testing.T, tenant string, roles, portfolios []string) string {
	return i.tokenFor(t, "agent:boundary-proof", tenant, roles, portfolios)
}

func (i *fixtureIssuer) tokenFor(t *testing.T, subject, tenant string, roles, portfolios []string) string {
	t.Helper()
	encode := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := encode(map[string]string{"alg": "RS256", "kid": "agent-boundary-fixture", "typ": "JWT"})
	claims := encode(map[string]any{
		"iss": i.server.URL, "aud": "agent-boundary", "sub": subject, "tenant": tenant,
		"roles": roles, "portfolios": portfolios, "iat": time.Now().Add(-time.Second).Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
	})
	payload := header + "." + claims
	hash := sha256.Sum256([]byte(payload))
	signature, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	return payload + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func request(t *testing.T, method, url, token, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	resp, err := proofClient().Do(r)
	if err != nil {
		t.Fatalf("fixture HTTP %s: %v", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(b) > 2<<20 {
		t.Fatal("fixture response could not be read within its bound")
	}
	return resp.StatusCode, b
}

func denied(status int) error {
	if status != http.StatusForbidden {
		return fmt.Errorf("capability gate returned %d; require 403 before the handler, including when a disabled publisher returns 503", status)
	}
	return nil
}
