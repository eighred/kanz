package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer() *Server {
	rd := &Readiness{}
	rd.Set(true)
	return New(rd, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestServer_Readyz(t *testing.T) {
	rec := do(t, newTestServer(), http.MethodGet, "/readyz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz: got %d", rec.Code)
	}
}

func TestServer_Returns(t *testing.T) {
	body := `{"portfolio_id":"P","sub_periods":[{"BeginValue":100,"Flow":0,"EndValue":110},{"BeginValue":110,"Flow":50,"EndValue":176}]}`
	rec := do(t, newTestServer(), http.MethodPost, "/v1/returns", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("returns: got %d body %s", rec.Code, rec.Body.String())
	}
	var resp returnsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if d := resp.TimeWeighted - 0.21; d > 1e-9 || d < -1e-9 {
		t.Fatalf("TWR: got %.6f want 0.21", resp.TimeWeighted)
	}
}

func TestServer_Attribution(t *testing.T) {
	body := `{"portfolio_id":"P","benchmark_id":"BM","sectors":[
		{"Sector":"A","PortfolioWeight":0.6,"BenchmarkWeight":0.5,"PortfolioReturn":0.10,"BenchmarkReturn":0.08},
		{"Sector":"B","PortfolioWeight":0.4,"BenchmarkWeight":0.5,"PortfolioReturn":0.05,"BenchmarkReturn":0.06}]}`
	rec := do(t, newTestServer(), http.MethodPost, "/v1/attribution", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("attribution: got %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Attribution struct {
			TotalAllocation, TotalSelection, TotalInteraction, ActiveReturn float64
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	a := resp.Attribution
	if d := (a.TotalAllocation + a.TotalSelection + a.TotalInteraction) - a.ActiveReturn; d > 1e-9 || d < -1e-9 {
		t.Fatalf("effects must reconcile to active return: %+v", a)
	}
}

func TestServer_Risk(t *testing.T) {
	body := `{"portfolio":[0.02,-0.04,0.06,-0.02,0.04],"benchmark":[0.01,-0.02,0.03,-0.01,0.02],"periods_per_year":252}`
	rec := do(t, newTestServer(), http.MethodPost, "/v1/risk", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("risk: got %d", rec.Code)
	}
}

func TestServer_BadBody(t *testing.T) {
	rec := do(t, newTestServer(), http.MethodPost, "/v1/risk", "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: got %d want 400", rec.Code)
	}
}
