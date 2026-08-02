package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eighred/kanz/internal/wealth"
	"github.com/eighred/kanz/services/wealth/internal/book"
)

// testTenant is the tenant this instance serves. Every request below must carry
// it in X-Kanz-Principal-Tenant or callerOwnsThisInstance refuses (#222) — the
// gateway injects that header on every forwarded request, and this surface used
// to ignore it entirely.
const testTenant = "acme"

func newServer(t *testing.T) (*Server, book.Store) {
	t.Helper()
	store := book.NewMemoryStore()
	r := &Readiness{}
	r.Set(true)
	return New(r, nil, testTenant, store), store
}

// asTenant builds a request the way the gateway forwards one: authenticated, with
// the caller's tenant injected.
func asTenant(method, path, tenant string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if tenant != "" {
		req.Header.Set(HeaderPrincipalTenant, tenant)
	}
	return req
}

func seed(t *testing.T, store book.Store) {
	t.Helper()
	h := wealth.Household{
		HouseholdID: "HH1",
		Accounts: []wealth.Account{
			{AccountID: "A1", Cash: 100, Holdings: []wealth.Holding{
				{InstrumentID: "VTI", AssetClass: "EQUITY", MarketValue: 600},
				{InstrumentID: "BND", AssetClass: "FIXED_INCOME", MarketValue: 300},
			}},
			{AccountID: "A2", Holdings: []wealth.Holding{
				{InstrumentID: "VTI", AssetClass: "EQUITY", MarketValue: 400},
				{InstrumentID: "BND", AssetClass: "FIXED_INCOME", MarketValue: 100},
			}},
		},
	}
	if err := store.Put(context.Background(), h); err != nil {
		t.Fatal(err)
	}
}

func TestHealthAndReady(t *testing.T) {
	s, _ := newServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, asTenant(http.MethodGet, path, testTenant))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200 got %d", path, rec.Code)
		}
	}
}

func TestHouseholdEndpoint(t *testing.T) {
	s, store := newServer(t)
	seed(t, store)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, asTenant(http.MethodGet, "/v1/households/HH1", testTenant))
	if rec.Code != http.StatusOK {
		t.Fatalf("household: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if tv, _ := out["total_value"].(float64); tv != 1500 {
		t.Fatalf("total_value: want 1500 got %v", out["total_value"])
	}
	holdings, _ := out["holdings"].(map[string]any)
	if vti, _ := holdings["VTI"].(float64); vti != 1000 {
		t.Fatalf("VTI holding: want 1000 got %v", holdings["VTI"])
	}
}

func TestHouseholdEndpoint_NotFound(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, asTenant(http.MethodGet, "/v1/households/UNKNOWN", testTenant))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown household: want 404 got %d", rec.Code)
	}
}
