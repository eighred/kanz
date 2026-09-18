package server

import (
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
)

func TestPostgresCashForecastEndpoint(t *testing.T) {
	pool := newServerPool(t)
	store := ledger.NewPostgres(pool)
	asOf := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if err := store.Append(t.Context(), &ledger.Event{EntryID: "cash", PortfolioID: "PF", Type: ledger.EntryCash, Cash: big.NewRat(12345, 100), CashCurrency: "USD", Effective: asOf, Knowledge: asOf, SettlementBasis: ledger.SettlementSettled, SettlementDate: asOf}, nil); err != nil {
		t.Fatal(err)
	}
	s := New(&Readiness{}, nil, store, "USD", WithTenant("__system__"))
	path := "/v1/portfolios/PF/cash-forecast?currency=USD&as_of=" + url.QueryEscape(asOf.Format(time.RFC3339Nano)) + "&horizon=" + url.QueryEscape(asOf.Add(24*time.Hour).Format(time.RFC3339Nano))
	for _, tenant := range []string{"", "other", "__system__"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set(auth.HeaderPrincipalTenant, tenant)
		r.Header.Set(auth.HeaderPrincipalSubject, "operator")
		if err := auth.SetPrincipalPortfolios(r.Header, []string{"PF"}); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if tenant != "__system__" {
			if w.Code != 404 {
				t.Fatalf("tenant %q: %d", tenant, w.Code)
			}
			continue
		}
		if w.Code != 200 {
			t.Fatalf("forecast: %d %s", w.Code, w.Body.String())
		}
		var f ledger.CashForecast
		if err := json.Unmarshal(w.Body.Bytes(), &f); err != nil {
			t.Fatal(err)
		}
		if f.Opening == nil || *f.Opening != "123.45" || f.Complete || f.SourceVersion == "" {
			t.Fatalf("invalid exact forecast: %s", w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", path, nil)
	auth.SetPrincipalHeaders(r.Header, "operator", "__system__", nil)
	if err := auth.SetPrincipalPortfolios(r.Header, []string{"OTHER"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("portfolio scope bypass: %d", w.Code)
	}
}
