package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/services/datamaster/internal/feed"
	"github.com/eighred/kanz/services/datamaster/internal/master"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
	"github.com/eighred/kanz/services/datamaster/internal/projector"
	"github.com/eighred/kanz/services/datamaster/internal/store"

	"github.com/eighred/kanz/pkg/auth"
)

func day(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

var now = day(2026, 6, 1)

func testFeeds() []feed.VendorFeed {
	return []feed.VendorFeed{
		feed.SimFeed{
			Name:     "BLOOMBERG",
			VRecords: []master.VendorRecord{{Vendor: "BLOOMBERG", InstrumentID: "INST1", Priority: 0, AssetClass: "EQUITY", CurrencyCode: "USD", Identifiers: master.Identifiers{ISIN: "US0000001"}, AsOf: now}},
			Candidates: []pricing.Candidate{
				{InstrumentID: "INST1", Source: "BLOOMBERG", Price: dec.Rat("100"), AsOf: now},
			},
		},
		feed.SimFeed{
			Name: "REFINITIV",
			Candidates: []pricing.Candidate{
				{InstrumentID: "INST1", Source: "REFINITIV", Price: dec.Rat("100"), AsOf: now},
				// A different instrument entirely. Its price must never reach INST1.
				{InstrumentID: "INST2", Source: "REFINITIV", Price: dec.Rat("999"), AsOf: now},
			},
		},
		feed.SimFeed{
			Name:     "ICE",
			VRecords: []master.VendorRecord{{Vendor: "ICE", InstrumentID: "INST1", Priority: 1, Description: "Apple Inc", Identifiers: master.Identifiers{ISIN: "US0000001"}, AsOf: now}},
			Candidates: []pricing.Candidate{
				{InstrumentID: "INST1", Source: "ICE", Price: dec.Rat("130"), AsOf: now}, // outlier ⇒ tolerance breach (median 100 outvotes it)
			},
		},
	}
}

// newServer wires the server the way the composition root does — over the two
// stores — and projects the feeds into the golden store first, exactly as the
// binary does before it becomes ready.
func newServer(t *testing.T) (*Server, store.ExceptionStore) {
	t.Helper()
	feeds := testFeeds()
	golden := store.NewMemoryGoldenStore()
	exceptions := store.NewQueueStore(pricing.NewQueue(), nil)
	proj := projector.New(feeds, golden, exceptions, nil, projector.WithClock(func() time.Time { return now }))
	if err := proj.Refresh(context.Background()); err != nil {
		t.Fatalf("projector refresh: %v", err)
	}
	r := &Readiness{}
	r.Set(true)
	return New(r, nil, testTenant, golden, exceptions, feeds, WithClock(func() time.Time { return now })), exceptions
}

// testTenant is the tenant these instances serve. Every request must carry it in
// X-Kanz-Principal-Tenant or callerOwnsThisInstance refuses (#222).
const testTenant = "acme"

// asTenant builds a request the way the gateway forwards one: authenticated,
// with the caller's tenant injected.
func asTenant(method, path, tenant string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if tenant != "" {
		req.Header.Set(auth.HeaderPrincipalTenant, tenant)
	}
	return req
}

// postAsTenant is asTenant for a request with a body.
func postAsTenant(path, tenant, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if tenant != "" {
		req.Header.Set(auth.HeaderPrincipalTenant, tenant)
	}
	return req
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, asTenant(http.MethodGet, path, testTenant))
	return rec
}

func TestHealthAndReady(t *testing.T) {
	s, _ := newServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := get(t, s, path); rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200 got %d", path, rec.Code)
		}
	}
}

func TestSecurityEndpoint(t *testing.T) {
	s, _ := newServer(t)
	rec := get(t, s, "/v1/securities/INST1")
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

	if rec := get(t, s, "/v1/securities/UNKNOWN"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown instrument: want 404 got %d", rec.Code)
	}
}

// errFeed fails every call — a vendor that is down.
type errFeed struct{}

func (errFeed) Vendor() string { return "DOWN" }
func (errFeed) Records(context.Context) ([]master.VendorRecord, error) {
	return nil, errors.New("vendor unreachable")
}
func (errFeed) Prices(context.Context) ([]pricing.Candidate, error) {
	return nil, errors.New("vendor unreachable")
}

// TestSecurityIsServedFromTheStore pins what this change bought.
//
// The golden record is read from the store, not resolved from the vendors inside
// the request. Every vendor here is DOWN, and the read still succeeds off the last
// projection. Before, this endpoint called master.Resolve over the live feeds on
// every request: reading an instrument twice made two rounds of vendor API calls,
// could return two different answers, and a vendor outage took the read API down
// with it.
func TestSecurityIsServedFromTheStore(t *testing.T) {
	golden := store.NewMemoryGoldenStore()
	if err := golden.Put(context.Background(), master.SecurityMaster{
		InstrumentID: "INST1", CurrencyCode: "USD", Description: "Apple Inc",
	}); err != nil {
		t.Fatal(err)
	}
	r := &Readiness{}
	r.Set(true)
	s := New(r, nil, testTenant, golden, store.NewQueueStore(nil, nil), []feed.VendorFeed{errFeed{}})

	rec := get(t, s, "/v1/securities/INST1")
	if rec.Code != http.StatusOK {
		t.Fatalf("the golden read hit the vendors: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["description"] != "Apple Inc" {
		t.Errorf("description = %v, want the projected record", out["description"])
	}
}

// TestPriceArbitratesOnlyItsOwnInstrument pins the second defect this fixed.
//
// A feed returns its whole price list. The price path used to hand ALL of it to
// Arbitrate for whichever instrument was asked for — so INST2's 999 was voted on
// as if it were a quote for INST1, and with enough instruments the consensus price
// of any one of them was a median of the whole book. Candidates now carry the
// instrument they are a price of, and the path filters on it.
func TestPriceArbitratesOnlyItsOwnInstrument(t *testing.T) {
	s, _ := newServer(t)
	rec := get(t, s, "/v1/prices/INST1")
	if rec.Code != http.StatusOK {
		t.Fatalf("price: want 200 got %d", rec.Code)
	}
	var out struct {
		Chosen   string `json:"chosen"` // an exact decimal string, never a JSON float
		HasPrice bool   `json:"has_price"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.HasPrice || out.Chosen != "100" {
		t.Fatalf("chosen = %q (has_price=%v), want the median of INST1's OWN candidates (100) — INST2's 999 must not vote",
			out.Chosen, out.HasPrice)
	}
}

func TestPriceAndOverrideFlow(t *testing.T) {
	s, exceptions := newServer(t)
	ctx := context.Background()

	// newServer's projector already filed the ICE tolerance break. Observation
	// reports the price without creating the evidence used by the override.
	if rec := get(t, s, "/v1/prices/INST1"); rec.Code != http.StatusOK {
		t.Fatalf("price: want 200 got %d", rec.Code)
	}
	open, err := exceptions.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for _, e := range open {
		if e.Kind == pricing.KindPriceTolerance && e.InstrumentID == "INST1" {
			id = e.ID
		}
	}
	if id == "" {
		t.Fatalf("want an open PRICE_TOLERANCE exception for INST1, got %v", open)
	}

	// Override it through the HTTP surface.
	body := `{"actor":"alice@kanz","reason":"corp action confirmed","chosen_price":"130"}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postAsPrincipal("/v1/exceptions/"+id+"/override", testTenant, "alice@kanz", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("override: want 200 got %d (%s)", rec.Code, rec.Body.String())
	}
	ex, ok, err := exceptions.Get(ctx, id)
	if err != nil || !ok {
		t.Fatalf("override read-back: ok=%v err=%v", ok, err)
	}
	if ex.Status != pricing.StatusOverridden || len(ex.Overrides) != 1 || ex.Overrides[0].Actor != "alice@kanz" {
		t.Errorf("override not recorded in the exception store: %+v", ex)
	}
	if ex.Overrides[0].ChosenPrice.Cmp(dec.Rat("130")) != 0 {
		t.Errorf("chosen price = %v, want exactly 130", ex.Overrides[0].ChosenPrice)
	}

	// A bad override is still rejected. The actor can no longer be missing — it is
	// the authenticated principal (#410) — so what this now pins is the price: an
	// override with no chosen_price is a decision with no number in it.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, postAsPrincipal("/v1/exceptions/"+id+"/override", testTenant, "alice@kanz", `{"reason":"x"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad override: want 400 got %d", rec.Code)
	}
}

// TestOverrideRefusesAJSONFloat pins the API contract DATA-M8b bought.
//
// The overridden price is what a NAMED HUMAN decided when accepting a break the
// system flagged, and it is written to an append-only audit trail. A JSON number
// is an IEEE-754 double by definition, so taking one would round that decision on
// its way into a compliance record — the same reason the regulatory filing API
// refuses a JSON float for money. It must arrive as an exact decimal string.
func TestOverrideRefusesAJSONFloat(t *testing.T) {
	s, exceptions := newServer(t)
	open, err := exceptions.Open(context.Background())
	if err != nil || len(open) == 0 {
		t.Fatalf("no exception to override: %v %v", open, err)
	}
	id := open[0].ID

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, postAsPrincipal("/v1/exceptions/"+id+"/override", testTenant, "alice@kanz", `{"actor":"alice@kanz","reason":"x","chosen_price":130.1}`))
	if rec.Code == http.StatusOK {
		t.Fatal("the API accepted a JSON float for the overridden price — a human's decision would be rounded into the audit trail")
	}

	// And a non-numeric string is not a price either.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, postAsPrincipal("/v1/exceptions/"+id+"/override", testTenant, "alice@kanz", `{"actor":"alice@kanz","reason":"x","chosen_price":"about a hundred"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unparseable price, got %d", rec.Code)
	}
}

// A price the platform's decimal scale cannot represent is refused, rather than
// stored exactly and then rounded by every surface that displays or publishes it.
// The audit record and the mark must be the same number.
func TestOverrideRefusesUnrepresentablePrecision(t *testing.T) {
	s, exceptions := newServer(t)
	open, err := exceptions.Open(context.Background())
	if err != nil || len(open) == 0 {
		t.Fatalf("no exception to override: %v %v", open, err)
	}
	id := open[0].ID

	post := func(price string) int {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, postAsPrincipal("/v1/exceptions/"+id+"/override", testTenant, "alice@kanz", `{"actor":"alice@kanz","reason":"x","chosen_price":"`+price+`"}`))
		return rec.Code
	}
	if code := post("130.123456789012345"); code != http.StatusBadRequest {
		t.Fatalf("a 15dp price was accepted (HTTP %d): it would be stored exactly and shown rounded", code)
	}
	if code := post("130.12345678"); code != http.StatusOK { // exactly at the platform scale
		t.Fatalf("a representable price was refused: HTTP %d", code)
	}
}
