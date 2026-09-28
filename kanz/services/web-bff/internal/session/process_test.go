package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These are separate production BFF processes over a real, forced-RLS store.
// Killing one process must neither lose the survivor's sessions nor resurrect revocations.
func TestBFFProcessesShareAuthorityAndFailClosed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	key := bytes.Repeat([]byte{12}, 32)
	m, e := NewPostgres(ctx, pool, key, time.Hour, DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	binary := os.Getenv("TEST_MFA_BFF")
	if binary == "" {
		binary = filepath.Join(t.TempDir(), "web-bff.exe")
		build := exec.Command("go", "build", "-o", binary, "../../cmd/web-bff")
		if out, e := build.CombinedOutput(); e != nil {
			t.Fatalf("build BFF: %v: %s", e, out)
		}
	}
	type process struct {
		base string
		stop func()
	}
	start := func() process {
		listener, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		addr := listener.Addr().String()
		_ = listener.Close()
		dsn, e := url.Parse(os.Getenv("TEST_POSTGRES_URL"))
		if e != nil {
			t.Fatal(e)
		}
		q := dsn.Query()
		q.Set("search_path", pool.Config().ConnConfig.RuntimeParams["search_path"])
		dsn.RawQuery = q.Encode()
		cmd := exec.Command(binary)
		for _, v := range os.Environ() {
			if !strings.HasPrefix(v, "WEB_BFF_") {
				cmd.Env = append(cmd.Env, v)
			}
		}
		cmd.Env = append(cmd.Env, "WEB_BFF_LISTEN="+addr, "WEB_BFF_IDENTITY_URL=http://127.0.0.1:1", "WEB_BFF_GATEWAY_URL=http://127.0.0.1:1", "WEB_BFF_INSECURE_COOKIES=1", "WEB_BFF_STATIC_DIR="+os.Getenv("TEST_MFA_STATIC_DIR"), "WEB_BFF_SESSION_MODE=postgres", "WEB_BFF_SESSION_DSN="+dsn.String(), "WEB_BFF_SESSION_KEY="+base64.StdEncoding.EncodeToString(key))
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		stopped := false
		stop := func() {
			if !stopped {
				stopped = true
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}
		t.Cleanup(stop)
		base := "http://" + addr
		client := &http.Client{Timeout: time.Second}
		ready := false
		for range 100 {
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
			t.Fatal("shared BFF failed readiness")
		}
		return process{base, stop}
	}
	a, b := start(), start()
	first, e := m.Create(ctx, Session{Subject: "alice", Tenant: "fund-a", AccessToken: "synthetic-access"})
	if e != nil {
		t.Fatal(e)
	}
	second, e := m.Create(ctx, Session{Subject: "alice", Tenant: "fund-a", AccessToken: "synthetic-access"})
	if e != nil {
		t.Fatal(e)
	}
	call := func(base, method, path, id string, want int) []byte {
		t.Helper()
		req, e := http.NewRequest(method, base+path, nil)
		if e != nil {
			t.Fatal(e)
		}
		if id != "" {
			req.AddCookie(&http.Cookie{Name: "kanz_session", Value: id})
		}
		client := &http.Client{Timeout: 5 * time.Second}
		resp, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer func() { _ = resp.Body.Close() }()
		body, e := io.ReadAll(resp.Body)
		if e != nil {
			t.Fatal(e)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: got %d want %d: %s", method, path, resp.StatusCode, want, body)
		}
		if want == 503 && len(resp.Cookies()) != 0 {
			t.Fatal("outage destroyed retryable cookie")
		}
		return body
	}
	call(a.base, "GET", "/auth/me", first, 200)
	call(b.base, "GET", "/auth/me", first, 200)
	raw := call(b.base, "GET", "/auth/sessions", first, 200)
	var inventory struct {
		Sessions []Summary `json:"sessions"`
	}
	if json.Unmarshal(raw, &inventory) != nil || len(inventory.Sessions) != 2 {
		t.Fatal("inventory missing")
	}
	call(a.base, "POST", "/auth/sessions/"+digest(second)+"/revoke", first, 200)
	call(b.base, "GET", "/auth/me", second, 401)
	a.stop()
	call(b.base, "GET", "/auth/me", first, 200)
	a = start()
	call(a.base, "GET", "/auth/me", first, 200)
	call(a.base, "GET", "/auth/me", second, 401)
	// An unavailable authority must fail closed on both reads and writes. A real
	// DB lock exercises pool/statement cancellation without killing unrelated tests.
	tx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, e = tx.Exec(ctx, `LOCK TABLE bff_session_slots IN ACCESS EXCLUSIVE MODE`); e != nil {
		t.Fatal(e)
	}
	call(a.base, "GET", "/readyz", "", 503)
	call(b.base, "GET", "/auth/me", first, 503)
	call(b.base, "POST", "/auth/logout", first, 503)
	if e = tx.Rollback(ctx); e != nil {
		t.Fatal(e)
	}
	call(a.base, "GET", "/readyz", "", 200)
	call(b.base, "GET", "/auth/me", first, 200)
	call(b.base, "POST", "/auth/logout", first, 200)
	call(a.base, "GET", "/auth/me", first, 401)
	if script := os.Getenv("TEST_SESSION_BROWSER_SCRIPT"); script != "" {
		browserID, e := m.Create(ctx, Session{Subject: "browser", Tenant: "fund-a", AccessToken: "synthetic"})
		if e != nil {
			t.Fatal(e)
		}
		otherID, e := m.Create(ctx, Session{Subject: "browser", Tenant: "fund-a", AccessToken: "synthetic"})
		if e != nil {
			t.Fatal(e)
		}
		input, _ := json.Marshal(map[string]string{"base": a.base, "replica": b.base, "cookie": browserID, "otherCookie": otherID})
		run, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(run, os.Getenv("TEST_MFA_NODE"), script)
		cmd.Stdin = bytes.NewReader(input)
		cmd.Stderr = os.Stderr
		if e = cmd.Run(); e != nil {
			t.Fatal("Chromium session management failed", e)
		}
		t.Log("Chromium shared-session inventory and revocation PASS")
	}

}
