package server

// THE NAV ENDPOINT ON THE ESTATE'S ACTUAL CONFIGURATION (#1041).
//
// Server.computeNAV took the completeness-gated path only when a request carried
// an `fx` table or the composition root had wired a live provider. WithLiveFX is
// appended only when ACCOUNTING_FX_PAIRS is non-empty, and NO MANIFEST IN THIS
// REPOSITORY SETS IT — so on every deployed pod fxProvider is nil and the
// unconverted arm was the only one a caller could reach. These tests construct
// exactly that server: no live FX, no `fx` in the request.

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
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

// unconfiguredServer is the estate's accounting pod: a book of record, a
// reporting currency, and no FX configuration of any kind.
func unconfiguredServer(t *testing.T, store ledger.Store) *Server {
	t.Helper()
	r := &Readiness{}
	r.Set(true)
	return New(r, nil, store, "USD", WithTenant(testTenant))
}

func seedTwoCurrencies(t *testing.T, store ledger.Store, portfolioID string) {
	t.Helper()
	eff := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, e := range []*ledger.Event{
		{EntryID: portfolioID + "-usd", PortfolioID: portfolioID, Type: ledger.EntryCash,
			Cash: big.NewRat(100, 1), CashCurrency: "USD", Effective: eff, Knowledge: eff, SourceRef: portfolioID + "-usd"},
		// One EUR subscription through POST /v1/portfolios/{id}/cash-movements,
		// whose Currency field is free-form, is all it takes to produce this book.
		{EntryID: portfolioID + "-eur", PortfolioID: portfolioID, Type: ledger.EntryCash,
			Cash: big.NewRat(50, 1), CashCurrency: "EUR", Effective: eff, Knowledge: eff, SourceRef: portfolioID + "-eur"},
	} {
		if err := store.Append(context.Background(), e, nil); err != nil {
			t.Fatalf("append %s: %v", e.EntryID, err)
		}
	}
}

func navAs(t *testing.T, s *Server, portfolioID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postV1(http.MethodPost, "/v1/portfolios/"+portfolioID+"/nav", strings.NewReader(body)))
	return rec
}

// THE ISSUE'S HTTP ACCEPTANCE CASE.
func TestNAVEndpointRefusesAMultiCurrencyBookWhenNoFXIsConfigured(t *testing.T) {
	store := ledger.NewMemoryStore()
	seedTwoCurrencies(t, store, "PF-MC")
	s := unconfiguredServer(t, store)

	rec := navAs(t, s, "PF-MC", `{"prices":{}}`)
	if rec.Code == http.StatusOK {
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		t.Fatalf("POST /v1/portfolios/PF-MC/nav answered 200 with total = %v over a book holding "+
			"USD 100 AND EUR 50 on a pod with no FX configured. The EUR is not in that number and "+
			"nothing in the response says so — an understated headline NAV is indistinguishable "+
			"from a correct one (#1041)", out["total"])
	}
	if !strings.Contains(rec.Body.String(), "EUR") {
		t.Fatalf("the refusal does not name EUR, so an operator cannot tell which rate is missing: %d %s",
			rec.Code, rec.Body.String())
	}
}

// THE POSITION LEG. A EUR-quoted holding was added into a USD total as a USD
// number — no currency check on the position leg existed at all. The join is
// wired here and FX is not, which is the state a deployment reaches the moment
// ACCOUNTING_INSTRUMENT_CURRENCY is set and ACCOUNTING_FX_PAIRS is not.
func TestNAVEndpointRefusesAForeignPositionWhenNoFXIsConfigured(t *testing.T) {
	store := ledger.NewMemoryStore()
	// EVERY CASH BUCKET IS USD ON PURPOSE. If the book also held EUR cash this
	// test would pass on the cash refusal and prove nothing about the position
	// leg, which is the half that had NO currency check at all.
	eff := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, e := range []*ledger.Event{
		{EntryID: "fp-c1", PortfolioID: "PF-FP", Type: ledger.EntryCash,
			Cash: big.NewRat(100000, 1), CashCurrency: "USD", Effective: eff, Knowledge: eff, SourceRef: "fp-c1"},
		{EntryID: "fp-t1", PortfolioID: "PF-FP", Type: ledger.EntryTrade, InstrumentID: "SAP",
			Quantity: big.NewRat(100, 1), Price: big.NewRat(120, 1),
			Cash: big.NewRat(-12000, 1), CashCurrency: "USD", Effective: eff, Knowledge: eff, SourceRef: "fp-t1"},
	} {
		if err := store.Append(context.Background(), e, nil); err != nil {
			t.Fatalf("append %s: %v", e.EntryID, err)
		}
	}
	r := &Readiness{}
	r.Set(true)
	s := New(r, nil, store, "USD", WithTenant(testTenant),
		WithInstrumentCurrency(accounting.InstrumentCurrency{"SAP": "EUR"}))

	rec := navAs(t, s, "PF-FP", `{"prices":{"SAP":"130"}}`)
	if rec.Code == http.StatusOK {
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		t.Fatalf("nav answered 200 with total = %v for a book whose only holding is EUR-quoted and "+
			"whose cash is all USD. 100 SAP at EUR 130 is EUR 13000, and it was added to a USD total "+
			"as a USD number — the position leg had no currency check at all (#1041)", out["total"])
	}
	if !strings.Contains(rec.Body.String(), "EUR") {
		t.Fatalf("the refusal does not name EUR: %d %s", rec.Code, rec.Body.String())
	}
}

// AND THE DOMESTIC BOOK STILL ANSWERS. The refusal must be reachable only for a
// book that genuinely holds a currency the pod cannot value; a pod with no FX
// configured serving a single-currency fund must keep answering, or this change
// takes every correctly-configured deployment offline.
func TestNAVEndpointStillValuesASingleCurrencyBookWithNoFXConfigured(t *testing.T) {
	store := ledger.NewMemoryStore()
	seed(t, store)
	s := unconfiguredServer(t, store)

	rec := navAs(t, s, "PF", `{"prices":{"AAPL":"160"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a domestic book on an FX-less pod must still value: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["total"] != "101000.00" {
		t.Fatalf("nav total: want 101000.00 got %v", out["total"])
	}
}

// THE SAME REFUSAL OVER A DURABLE JOURNAL UNDER RLS.
//
// The in-memory store beside this one folds a book from events it holds in a
// map. It cannot prove the thing the refusal actually depends on: that
// cash_currency round-trips through Postgres, so the FOLDED book really does
// carry a second bucket. A column that came back empty would collapse every
// currency into one key — and a one-bucket book values cleanly, so the endpoint
// would answer 200 with an understated total and every in-memory test here
// would still be green.
//
// Gated on TEST_POSTGRES_URL, and the role must be NOSUPERUSER or RLS is
// bypassed and the tenant pinning proves nothing.
func TestPostgresNAVEndpointRefusesAMultiCurrencyBookWhenNoFXIsConfigured(t *testing.T) {
	pool := newServerPool(t)
	store := ledger.NewPostgres(pool)
	const portfolioID = "PF-MC-PG"
	seedTwoCurrencies(t, store, portfolioID)

	// PROVE THE JOURNAL REALLY CAME BACK WITH TWO BUCKETS before asserting on the
	// endpoint, so a refusal produced by some other failure cannot be mistaken for
	// this one.
	book, _, err := ledger.MaterializeCurrent(context.Background(), store, portfolioID)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(book.Cash) != 2 || book.Cash["USD"] == nil || book.Cash["EUR"] == nil {
		t.Fatalf("the durable fold produced %d cash bucket(s) (%v) — cash_currency did not "+
			"round-trip, so this test would be asserting on a single-currency book", len(book.Cash), book.Cash)
	}

	s := unconfiguredServer(t, store)
	rec := navAs(t, s, portfolioID, `{"prices":{}}`)
	if rec.Code == http.StatusOK {
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		t.Fatalf("nav over a DURABLE two-currency journal answered 200 with total = %v", out["total"])
	}
	if !strings.Contains(rec.Body.String(), "EUR") {
		t.Fatalf("the refusal does not name EUR: %d %s", rec.Code, rec.Body.String())
	}
	t.Logf("durable two-currency book refused: %d %s", rec.Code, strings.TrimSpace(rec.Body.String()))

	// AND THE DOMESTIC BOOK IN THE SAME DATABASE STILL VALUES, so the refusal is
	// not "Postgres reads fail" wearing a currency name.
	seed(t, store)
	ok := navAs(t, s, "PF", `{"prices":{"AAPL":"160"}}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("a domestic book on the same durable store must still value: %d %s", ok.Code, ok.Body.String())
	}
}
