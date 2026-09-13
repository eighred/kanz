package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/wealth"
	"github.com/eighred/kanz/services/wealth/internal/book"

	"github.com/eighred/kanz/pkg/auth"
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
		req.Header.Set(auth.HeaderPrincipalTenant, tenant)
	}
	return req
}

func seed(t *testing.T, store book.Store) {
	t.Helper()
	h := wealth.Household{
		HouseholdID: "HH1", CurrencyCode: "USD", AsOf: time.Unix(1700000000, 0), RecordedBy: "test:advisor", Reason: "test valuation",
		Accounts: []wealth.Account{
			{AccountID: "A1", Cash: "100", Holdings: []wealth.Holding{
				{InstrumentID: "VTI", AssetClass: "EQUITY", MarketValue: "600"},
				{InstrumentID: "BND", AssetClass: "FIXED_INCOME", MarketValue: "300"},
			}},
			{AccountID: "A2", Cash: "0", Holdings: []wealth.Holding{
				{InstrumentID: "VTI", AssetClass: "EQUITY", MarketValue: "400"},
				{InstrumentID: "BND", AssetClass: "FIXED_INCOME", MarketValue: "100"},
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
	if tv, _ := out["total_value"].(string); tv != "1500" {
		t.Fatalf("total_value: want 1500 got %v", out["total_value"])
	}
	holdings, _ := out["holdings"].(map[string]any)
	if vti, _ := holdings["VTI"].(string); vti != "1000" {
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

func TestExactHouseholdContractAndZeroWeights(t *testing.T) {
	s, st := newServer(t)
	h := wealth.Household{HouseholdID: "fraction", CurrencyCode: "USD", AsOf: time.Unix(1700000000, 0), RecordedBy: "test:advisor", Reason: "test source", Accounts: []wealth.Account{{AccountID: "a", Cash: "2", Holdings: []wealth.Holding{{InstrumentID: "x", AssetClass: "EQUITY", MarketValue: "1"}}}}}
	if err := st.Put(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"v1", "v2"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, asTenant("GET", "/"+version+"/households/fraction", testTenant))
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["arithmetic_version"] != float64(2) || body["total_value"] != "3" || body["cash"] != "2" || body["currency_code"] != "USD" || body["weights_state"] != "exact" || body["weights"].(map[string]any)["x"] != "1/3" {
			t.Fatal(body)
		}
		foreign := httptest.NewRecorder()
		s.ServeHTTP(foreign, asTenant("GET", "/"+version+"/households/fraction", "other"))
		if foreign.Code != 404 {
			t.Fatal("foreign read", foreign.Code)
		}
	}
	h.Accounts[0].Cash = "-1"
	if err := st.Put(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, asTenant("GET", "/v2/households/fraction", testTenant))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["total_value"] != "0" || body["weights_state"] != "unavailable" || body["weights"] != nil || body["asset_class"] != nil {
		t.Fatal("zero misrepresented", body)
	}
}
