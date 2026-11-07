package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/services/web-bff/internal/session"
)

// Only the negative-control server changes CSP. Both serve the actual BFF
// handler and production dist, not a second static-server implementation.
type oldRendererCSP struct {
	http.ResponseWriter
	wrote bool
}

func (w *oldRendererCSP) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.Header().Set("Content-Security-Policy", strings.Replace(browserCSP, " 'wasm-unsafe-eval'", "", 1))
	w.ResponseWriter.WriteHeader(code)
}
func (w *oldRendererCSP) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.ResponseWriter.Write(p)
}

func TestBrowserRendererProductionBundle(t *testing.T) {
	script := os.Getenv("TEST_RENDER_BROWSER_SCRIPT")
	if script == "" {
		t.Skip("requires production dist and actual Chromium")
	}
	var requests atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/portfolios/fixture/orders" || r.Header.Get("Authorization") != "Bearer synthetic-render-fixture" || r.Header.Get("Cookie") != "" {
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"unindexed": "0", "orders": []any{
			map[string]any{"order_id": "canonical", "quantity": map[string]any{"coefficient": "9007199254740993", "exponent": -2}, "filled_quantity": map[string]any{"coefficient": "-0", "exponent": -2}, "limit_price": map[string]any{"amount": map[string]any{"coefficient": "150", "exponent": -2}, "currency_code": "USD"}},
			map[string]any{"order_id": "legacy", "quantity": map[string]any{"coefficient": strings.Repeat("9", 129), "exponent": -2}},
		}})
	}))
	defer gateway.Close()
	base := bffWithIdentity(t, "http://127.0.0.1:1", gateway.URL)
	defer base.Close()
	srv, err := New(&Readiness{}, Options{Identity: base.identity, ClientIP: base.clientIP, Sessions: base.sessions,
		GatewayURL: gateway.URL, StaticDir: os.Getenv("TEST_MFA_STATIC_DIR"), SecureCookies: false})
	if err != nil || srv.static == nil {
		t.Fatal("production dist required")
	}
	defer srv.Close()
	cookie, err := srv.sessions.Create(context.Background(), session.Session{Subject: "renderer-fixture", Tenant: "fixture", AccessToken: "synthetic-render-fixture", Expiry: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	network := httptest.NewServer(srv)
	defer network.Close()
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { srv.ServeHTTP(&oldRendererCSP{ResponseWriter: w}, r) }))
	defer old.Close()
	input, _ := json.Marshal(map[string]string{"base": network.URL, "old": old.URL, "cookie": cookie})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Getenv("TEST_MFA_NODE"), script)
	command.Stdin = bytes.NewReader(input)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		t.Fatal("actual browser renderer qualification failed")
	}
	if requests.Load() != 5 {
		t.Fatal("browser scenarios did not traverse the authenticated BFF")
	}
}
