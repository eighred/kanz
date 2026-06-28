package server

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/services/accounting/internal/ledger"
)

func newServer(t *testing.T) (*Server, ledger.Store) {
	t.Helper()
	store := ledger.NewMemoryStore()
	r := &Readiness{}
	r.Set(true)
	return New(r, nil, store, "USD"), store
}

func seed(t *testing.T, store ledger.Store) {
	t.Helper()
	eff := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	entries := []*ledger.Event{
		{EntryID: "c1", PortfolioID: "PF", Type: ledger.EntryCash, Cash: big.NewRat(100000, 1), CashCurrency: "USD", Effective: eff, Knowledge: eff},
		{EntryID: "t1", PortfolioID: "PF", Type: ledger.EntryTrade, InstrumentID: "AAPL",
			Quantity: big.NewRat(100, 1), Price: big.NewRat(150, 1), Cash: big.NewRat(-15000, 1), CashCurrency: "USD",
			Effective: eff, Knowledge: eff},
	}
	for _, e := range entries {
		if err := store.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHealthAndReady(t *testing.T) {
	s, _ := newServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200 got %d", path, rec.Code)
		}
	}
}

func TestNAVEndpoint(t *testing.T) {
	s, store := newServer(t)
	seed(t, store)
	body := `{"prices":{"AAPL":"160"}}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/portfolios/PF/nav", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("nav: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// total = 85000 cash + 16000 sec = 101000
	if out["total"] != "101000.00" {
		t.Fatalf("nav total: want 101000.00 got %v", out["total"])
	}
}

func TestReconcileEndpoint(t *testing.T) {
	s, store := newServer(t)
	seed(t, store)
	body := `{"positions":{"AAPL":"90"},"cash":{"USD":"85000"}}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/portfolios/PF/reconcile", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("reconcile: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// AAPL quantity break (100 vs 90); cash matches; MSFT absent both sides.
	if out.Count != 1 {
		t.Fatalf("reconcile breaks: want 1 got %d (%s)", out.Count, rec.Body.String())
	}
}

func TestNAVMissingPrice(t *testing.T) {
	s, store := newServer(t)
	seed(t, store)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/portfolios/PF/nav", strings.NewReader(`{"prices":{}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("nav missing price: want 400 got %d", rec.Code)
	}
}
