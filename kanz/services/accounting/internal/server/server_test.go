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

	accounting "github.com/eighred/kanz/services/accounting/internal"
	"github.com/eighred/kanz/services/accounting/internal/cashmove"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

func newServer(t *testing.T) (*Server, ledger.Store) {
	t.Helper()
	store := ledger.NewMemoryStore()
	r := &Readiness{}
	r.Set(true)
	// EVERY SERVER IN THESE TESTS CARRIES THE CUSTODY DECLARATION, because the
	// reconcile endpoint refuses without one (#1025) — see testScope, and
	// TestReconcileFailsClosedWithNoCustodyScopeWired for the other direction.
	return New(r, nil, store, "USD", WithTenant(testTenant), WithCustodyBookScope(testScope(t))), store
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
		if err := store.Append(context.Background(), e, nil); err != nil {
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
	s.ServeHTTP(rec, postV1(http.MethodPost, "/v1/portfolios/PF/nav", strings.NewReader(body)))
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
	// PF has ONE configured custodian, so the whole book IS that custodian's book —
	// the pre-#1006 behaviour, which is correct here and does not move. The request
	// still names it: a custodian is never inferred (#1025).
	body := `{"custodian_id":"CUST-A","positions":{"AAPL":"90"},"cash":{"USD":"85000"}}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postV1(http.MethodPost, "/v1/portfolios/PF/reconcile", strings.NewReader(body)))
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

// seedEUR seeds a book holding a EUR-quoted instrument + EUR cash, so a USD NAV
// needs an FX conversion for EUR.
func seedEUR(t *testing.T, store ledger.Store) {
	t.Helper()
	eff := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	entries := []*ledger.Event{
		{EntryID: "c1", PortfolioID: "PF", Type: ledger.EntryCash, Cash: big.NewRat(10000, 1), CashCurrency: "EUR", Effective: eff, Knowledge: eff},
		{EntryID: "t1", PortfolioID: "PF", Type: ledger.EntryTrade, InstrumentID: "SAP",
			Quantity: big.NewRat(100, 1), Price: big.NewRat(120, 1), Cash: big.NewRat(-12000, 1), CashCurrency: "EUR",
			Effective: eff, Knowledge: eff},
	}
	for _, e := range entries {
		if err := store.Append(context.Background(), e, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// WIRE-01d: with a live FX provider + default instrument→currency join wired,
// the NAV endpoint values a multi-currency book with no `fx` in the request.
func TestNAVLiveFXWithoutRequestFX(t *testing.T) {
	store := ledger.NewMemoryStore()
	seedEUR(t, store)
	r := &Readiness{}
	r.Set(true)
	// EUR = 1.10 USD; SAP is a EUR instrument.
	fx := accounting.NewFXTable("USD", map[string]*big.Rat{"EUR": big.NewRat(110, 100)})
	s := New(r, nil, store, "USD", WithTenant(testTenant),
		WithLiveFX(func() accounting.FXConverter { return fx }),
		WithInstrumentCurrency(accounting.InstrumentCurrency{"SAP": "EUR"}),
	)

	rec := httptest.NewRecorder()
	// No "fx" and no "instrument_currency" in the request — both come from the
	// live provider + server default.
	s.ServeHTTP(rec, postV1(http.MethodPost, "/v1/portfolios/PF/nav", strings.NewReader(`{"prices":{"SAP":"130"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("nav: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// EUR book = (10000 − 12000) cash + 13000 sec = 11000 EUR × 1.10 = 12100 USD.
	if out["total"] != "12100.00" {
		t.Fatalf("nav total: want 12100.00 got %v (%s)", out["total"], rec.Body.String())
	}
	if out["currency"] != "USD" {
		t.Fatalf("nav currency: want USD got %v", out["currency"])
	}
}

// A per-request `fx` still overrides the live provider (client override wins).
func TestNAVRequestFXOverridesLive(t *testing.T) {
	store := ledger.NewMemoryStore()
	seedEUR(t, store)
	r := &Readiness{}
	r.Set(true)
	liveFX := accounting.NewFXTable("USD", map[string]*big.Rat{"EUR": big.NewRat(110, 100)})
	s := New(r, nil, store, "USD", WithTenant(testTenant),
		WithLiveFX(func() accounting.FXConverter { return liveFX }),
		WithInstrumentCurrency(accounting.InstrumentCurrency{"SAP": "EUR"}),
	)
	rec := httptest.NewRecorder()
	// Request supplies EUR = 1.00 → total should use 1.00, not the live 1.10.
	s.ServeHTTP(rec, postV1(http.MethodPost, "/v1/portfolios/PF/nav", strings.NewReader(`{"prices":{"SAP":"130"},"fx":{"EUR":"1"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("nav: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// 11000 EUR × 1.00 = 11000 USD (live 1.10 would give 12100).
	if out["total"] != "11000.00" {
		t.Fatalf("request fx should override live: want 11000.00 got %v", out["total"])
	}
}

// A foreign holding whose currency has no live rate fails the valuation loudly
// (completeness-gated) rather than mis-valuing.
func TestNAVLiveFXMissingRateFails(t *testing.T) {
	store := ledger.NewMemoryStore()
	seedEUR(t, store)
	r := &Readiness{}
	r.Set(true)
	// Live table has no EUR rate yet.
	empty := accounting.NewFXTable("USD", nil)
	s := New(r, nil, store, "USD", WithTenant(testTenant),
		WithLiveFX(func() accounting.FXConverter { return empty }),
		WithInstrumentCurrency(accounting.InstrumentCurrency{"SAP": "EUR"}),
	)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postV1(http.MethodPost, "/v1/portfolios/PF/nav", strings.NewReader(`{"prices":{"SAP":"130"}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing FX rate: want 400 got %d (%s)", rec.Code, rec.Body.String())
	}
}

type fakeCashPublisher struct {
	got cashmove.CashMovement
	err error
}

func (f *fakeCashPublisher) Publish(_ context.Context, m cashmove.CashMovement) error {
	f.got = m
	return f.err
}

// WIRE-01f: a POST to the cash-movement endpoint emits the movement (202) rather
// than writing the store — the event-sourced path — carrying the parsed kind,
// amount, currency, and the portfolio from the path.
func TestCashMovementEndpointEmits(t *testing.T) {
	pub := &fakeCashPublisher{}
	r := &Readiness{}
	r.Set(true)
	s := New(r, nil, ledger.NewMemoryStore(), "USD", WithTenant(testTenant), WithCashPublisher(pub))

	body := `{"movement_id":"S1","kind":"subscription","amount":"100000","source_ref":"wealth-42"}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postV1(http.MethodPost, "/v1/portfolios/PF/cash-movements", strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202 got %d (%s)", rec.Code, rec.Body.String())
	}
	if pub.got.MovementID != "S1" || pub.got.PortfolioID != "PF" || pub.got.Kind != cashmove.Subscription {
		t.Fatalf("published movement wrong: %+v", pub.got)
	}
	if pub.got.Currency != "USD" { // defaulted to base currency
		t.Fatalf("currency = %q, want USD default", pub.got.Currency)
	}
	if pub.got.Amount == nil || pub.got.Amount.Sign() <= 0 {
		t.Fatalf("amount not parsed: %v", pub.got.Amount)
	}
}

// An unknown kind is a 400 before any emit.
func TestCashMovementRejectsUnknownKind(t *testing.T) {
	pub := &fakeCashPublisher{}
	r := &Readiness{}
	r.Set(true)
	s := New(r, nil, ledger.NewMemoryStore(), "USD", WithTenant(testTenant), WithCashPublisher(pub))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postV1(http.MethodPost, "/v1/portfolios/PF/cash-movements", strings.NewReader(`{"movement_id":"X","kind":"bonus","amount":"1"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", rec.Code)
	}
}

// Without a publisher wired, the endpoint is not mounted (404).
func TestCashMovementNotMountedWithoutPublisher(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postV1(http.MethodPost, "/v1/portfolios/PF/cash-movements", strings.NewReader(`{"movement_id":"X","kind":"subscription","amount":"1"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 (endpoint not mounted) got %d", rec.Code)
	}
}

func TestNAVMissingPrice(t *testing.T) {
	s, store := newServer(t)
	seed(t, store)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postV1(http.MethodPost, "/v1/portfolios/PF/nav", strings.NewReader(`{"prices":{}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("nav missing price: want 400 got %d", rec.Code)
	}
}
