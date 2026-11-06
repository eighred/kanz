package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/tv-sync/internal/brokerapi"
	"github.com/eighred/kanz/services/tv-sync/internal/projection"
)

func TestOperationsListenerCannotServeBrokerRoutes(t *testing.T) {
	readiness := &Readiness{}
	api := New(readiness, brokerapi.New(projection.New(time.Now, nil)), WithMetrics(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fixture_metric 1\n")
	})))
	apiHTTP := httptest.NewServer(api)
	defer apiHTTP.Close()
	operationsHTTP := httptest.NewServer(api.OperationsHandler())
	defer operationsHTTP.Close()
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	get := func(base, path, tenant string, want int) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tenant != "" {
			req.Header.Set(auth.HeaderPrincipalTenant, tenant)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("GET %s: got %d, want %d", req.URL, res.StatusCode, want)
		}
	}
	for _, path := range []string{
		"/broker/config", "/broker/accounts", "/broker/accounts/fund/state",
		"/broker/accounts/fund/positions", "/broker/accounts/fund/orders",
		"/broker/accounts/fund/executions", "/broker/accounts/fund/stream",
	} {
		// Even a forged gateway principal cannot select a tenant route on the
		// monitoring socket. This is route isolation, not network-policy proof.
		get(operationsHTTP.URL, path, "acme", http.StatusNotFound)
	}
	get(apiHTTP.URL, "/broker/config", "", http.StatusServiceUnavailable)
	readiness.Set(true)
	get(apiHTTP.URL, "/broker/config", "", http.StatusOK)
	get(apiHTTP.URL, "/broker/accounts", "acme", http.StatusOK)
	get(apiHTTP.URL, "/broker/accounts", "", http.StatusUnauthorized)
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		get(apiHTTP.URL, path, "acme", http.StatusNotFound)
	}
	get(operationsHTTP.URL, "/healthz", "", http.StatusOK)
	get(operationsHTTP.URL, "/metrics", "", http.StatusOK)
	readiness.Set(false)
	get(operationsHTTP.URL, "/readyz", "", http.StatusServiceUnavailable)
	readiness.Set(true)
	get(operationsHTTP.URL, "/readyz", "", http.StatusOK)
	readiness.Set(false)
	get(operationsHTTP.URL, "/readyz", "", http.StatusServiceUnavailable)
}
