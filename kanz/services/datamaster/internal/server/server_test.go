package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/services/datamaster/internal/feed"
	"github.com/kanz-eng/kanz/services/datamaster/internal/master"
	"github.com/kanz-eng/kanz/services/datamaster/internal/pricing"
)

func day(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

func newServer(t *testing.T) (*Server, *pricing.Queue) {
	t.Helper()
	now := day(2026, 6, 1)
	feeds := []feed.VendorFeed{
		feed.SimFeed{
			Name:     "BLOOMBERG",
			VRecords: []master.VendorRecord{{Vendor: "BLOOMBERG", InstrumentID: "INST1", Priority: 0, AssetClass: "EQUITY", CurrencyCode: "USD", Identifiers: master.Identifiers{ISIN: "US0000001"}, AsOf: now}},
			Candidates: []pricing.Candidate{
				{Source: "BLOOMBERG", Price: 100, AsOf: now},
			},
		},
		feed.SimFeed{
			Name: "REFINITIV",
			Candidates: []pricing.Candidate{
				{Source: "REFINITIV", Price: 100, AsOf: now},
			},
		},
		feed.SimFeed{
			Name:     "ICE",
			VRecords: []master.VendorRecord{{Vendor: "ICE", InstrumentID: "INST1", Priority: 1, Description: "Apple Inc", Identifiers: master.Identifiers{ISIN: "US0000001"}, AsOf: now}},
			Candidates: []pricing.Candidate{
				{Source: "ICE", Price: 130, AsOf: now}, // outlier ⇒ tolerance breach (median 100 outvotes it)
			},
		},
	}
	q := pricing.NewQueue()
	r := &Readiness{}
	r.Set(true)
	return New(r, nil, feeds, q, WithClock(func() time.Time { return now })), q
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

func TestSecurityEndpoint(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/securities/INST1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("security: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["currency_code"] != "USD" {
		t.Errorf("currency = %v, want USD", out["currency_code"])
	}
	if out["description"] != "Apple Inc" {
		t.Errorf("description = %v, want Apple Inc (survived from ICE)", out["description"])
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/securities/UNKNOWN", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown instrument: want 404 got %d", rec.Code)
	}
}

func TestPriceAndOverrideFlow(t *testing.T) {
	s, q := newServer(t)

	// Arbitrate: the ICE outlier breaches tolerance ⇒ one open exception.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/prices/INST1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("price: want 200 got %d", rec.Code)
	}
	open := q.Open()
	if len(open) != 1 || open[0].Kind != pricing.KindPriceTolerance {
		t.Fatalf("want 1 open PRICE_TOLERANCE exception, got %v", open)
	}
	id := open[0].ID

	// Override it through the HTTP surface.
	body := `{"actor":"alice@kanz","reason":"corp action confirmed","chosen_price":130}`
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/exceptions/"+id+"/override", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("override: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(q.Open()) != 0 {
		t.Errorf("overridden exception should leave the open queue")
	}
	ex, _ := q.Get(id)
	if ex.Status != pricing.StatusOverridden || len(ex.Overrides) != 1 {
		t.Errorf("override not recorded: %+v", ex)
	}

	// A bad override (missing actor) is rejected.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/exceptions/"+id+"/override", strings.NewReader(`{"reason":"x"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad override: want 400 got %d", rec.Code)
	}
}
