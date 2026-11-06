package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/tv-sync/internal/brokerapi"
	"github.com/eighred/kanz/services/tv-sync/internal/projection"
)

func TestBrokerRefusesBeforeReadinessAndAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := NewReadiness(ctx)
	s := New(ready, brokerapi.New(projection.New(time.Now, nil)))
	check := func(h http.Handler, path string, want int) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set(auth.HeaderPrincipalTenant, "fixture")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s status%d want%d", path, w.Code, want)
		}
	}
	for _, h := range []http.Handler{s, s.LegacyHandler()} {
		check(h, "/broker/accounts", 503)
		check(h, "/broker/config", 503)
	}
	check(s.OperationsHandler(), "/healthz", 200)
	check(s.OperationsHandler(), "/readyz", 503)
	ready.Set(true)
	for _, h := range []http.Handler{s, s.LegacyHandler()} {
		check(h, "/broker/accounts", 200)
	}
	cancel()
	ready.Set(true)
	for _, h := range []http.Handler{s, s.LegacyHandler()} {
		check(h, "/broker/accounts", 503)
	}
	check(s.OperationsHandler(), "/readyz", 503)
}
