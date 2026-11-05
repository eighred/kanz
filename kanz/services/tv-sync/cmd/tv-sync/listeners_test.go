package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/tv-sync/internal/brokerapi"
	"github.com/eighred/kanz/services/tv-sync/internal/config"
	"github.com/eighred/kanz/services/tv-sync/internal/projection"
	"github.com/eighred/kanz/services/tv-sync/internal/server"
)

func TestBindHTTPListenersClosesOperationsWhenAPIBindFails(t *testing.T) {
	// Reserve then release the first address so it can be checked for leakage
	// after the second bind fails against a real occupied socket.
	lc := net.ListenConfig{}
	occupied, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	first, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	operationsAddr := first.Addr().String()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	api := server.New(&server.Readiness{}, brokerapi.New(projection.New(time.Now, nil)))
	servers := newHTTPServers(config.Config{Listen: operationsAddr, APIListen: occupied.Addr().String()}, api)
	listeners, err := bindHTTPListeners(context.Background(), servers)
	if err == nil {
		for _, ln := range listeners {
			_ = ln.Close()
		}
		t.Fatal("occupied API address did not refuse startup")
	}
	if !strings.Contains(err.Error(), occupied.Addr().String()) {
		t.Fatalf("startup did not reach the API bind: %v", err)
	}
	if listeners != nil {
		t.Fatal("failed startup returned listeners")
	}
	rebound, err := lc.Listen(context.Background(), "tcp", operationsAddr)
	if err != nil {
		t.Fatalf("operations socket leaked after failed startup: %v", err)
	}
	_ = rebound.Close()
}

func TestHTTPServerModesRetainLegacyUntilExplicitSplit(t *testing.T) {
	for _, split := range []bool{false, true} {
		name := "legacy"
		if split {
			name = "split"
		}
		t.Run(name, func(t *testing.T) {
			readiness := &server.Readiness{}
			api := server.New(readiness, brokerapi.New(projection.New(time.Now, nil)), server.WithMetrics(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "fixture_metric 1\n")
			})))
			cfg := config.Config{Listen: ":8091"}
			wantListeners := 1
			if split {
				cfg.APIListen = ":8113"
				wantListeners = 2
			}
			servers := newHTTPServers(cfg, api)
			if len(servers) != wantListeners {
				t.Fatalf("listeners=%d want%d", len(servers), wantListeners)
			}
			if servers[0].Addr != cfg.Listen {
				t.Fatal("operations address changed")
			}
			operations := httptest.NewServer(servers[0].Handler)
			defer operations.Close()
			brokerURL := operations.URL
			if split {
				if servers[1].Addr != cfg.APIListen {
					t.Fatal("API address changed")
				}
				brokerHTTP := httptest.NewServer(servers[1].Handler)
				defer brokerHTTP.Close()
				brokerURL = brokerHTTP.URL
			}
			client := &http.Client{Timeout: 2 * time.Second}
			defer client.CloseIdleConnections()
			get := func(base, path string, want int) {
				t.Helper()
				req, err := http.NewRequest(http.MethodGet, base+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set(auth.HeaderPrincipalTenant, "acme")
				res, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				if res.StatusCode != want {
					t.Fatalf("GET %s =%d want%d", req.URL, res.StatusCode, want)
				}
			}
			get(operations.URL, "/healthz", http.StatusOK)
			get(operations.URL, "/metrics", http.StatusOK)
			get(operations.URL, "/readyz", http.StatusServiceUnavailable)
			readiness.Set(true)
			get(operations.URL, "/readyz", http.StatusOK)
			get(brokerURL, "/broker/accounts", http.StatusOK)
			get(brokerURL, "/broker/config", http.StatusOK)
			if split {
				get(operations.URL, "/broker/accounts", http.StatusNotFound)
				get(brokerURL, "/metrics", http.StatusNotFound)
			}
		})
	}
}
