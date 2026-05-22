package server_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kanz-eng/kanz/services/risk-engine/internal/server"
)

func newTestServer(t *testing.T) (*server.Readiness, *httptest.Server) {
	t.Helper()
	readiness := &server.Readiness{}
	ts := httptest.NewServer(server.New(readiness, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	return readiness, ts
}

func get(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp.StatusCode
}

func TestHealthzAlwaysOK(t *testing.T) {
	_, ts := newTestServer(t)
	// Liveness reports the process is up regardless of readiness state.
	if code := get(t, ts.URL+"/healthz"); code != http.StatusOK {
		t.Fatalf("healthz status=%d want 200", code)
	}
}

func TestReadyzReflectsGate(t *testing.T) {
	readiness, ts := newTestServer(t)

	// Gate starts closed → 503.
	if code := get(t, ts.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz (not ready) status=%d want 503", code)
	}

	readiness.Set(true)
	if code := get(t, ts.URL+"/readyz"); code != http.StatusOK {
		t.Fatalf("readyz (ready) status=%d want 200", code)
	}

	// Draining (ORCH-01e) clears the gate → 503 again.
	readiness.Set(false)
	if code := get(t, ts.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz (draining) status=%d want 503", code)
	}
}
