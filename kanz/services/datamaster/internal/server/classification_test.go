package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/eighred/kanz/services/datamaster/internal/feed"
	"github.com/eighred/kanz/services/datamaster/internal/master"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
	"github.com/eighred/kanz/services/datamaster/internal/projector"
	"github.com/eighred/kanz/services/datamaster/internal/store"
)

// THE READ PATH SERVES THE CLASSIFICATION IT MASTERS (#640).
//
// handleSecurity resolved sector and issuer, persisted them in golden_records,
// and then dropped both on the way out — so the only read path to the security
// master could not answer "what sector is this" at all. That is half of why
// every SECTOR and ISSUER mandate on this estate was unresolvable: the data
// existed one process away with no way to ask for it. internal/refdata is the
// caller these fields exist for.

// serverOver wires the server the way newServer does — projecting the feeds
// into the golden store first, exactly as the binary does before it becomes
// ready — over a caller-supplied feed set, so each test can state the exact
// classification it is about.
func serverOver(t *testing.T, feeds []feed.VendorFeed) *Server {
	t.Helper()
	golden := store.NewMemoryGoldenStore()
	exceptions := store.NewQueueStore(pricing.NewQueue(), nil)
	proj := projector.New(feeds, golden, exceptions, nil, projector.WithClock(func() time.Time { return now }))
	if err := proj.Refresh(context.Background()); err != nil {
		t.Fatalf("projector refresh: %v", err)
	}
	r := &Readiness{}
	r.Set(true)
	return New(r, nil, testTenant, golden, exceptions, feeds, WithClock(func() time.Time { return now }))
}

// readSecurity does the projection the composition root does, then reads one
// instrument back through the HTTP surface.
func readSecurity(t *testing.T, s *Server, id string) map[string]any {
	t.Helper()
	rec := get(t, s, "/v1/securities/"+id)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/securities/%s: want 200 got %d (%s)", id, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestSecurityEndpointServesSectorIssuerAndAsOf(t *testing.T) {
	s, _ := newServer(t)
	out := readSecurity(t, s, "INST1")

	// SECTOR IS AN OBJECT AND IS ALWAYS PRESENT. An absent key and an empty value
	// would be the same on the wire, and they are different facts — the master
	// resolved no sector for this instrument, versus this endpoint does not serve
	// sectors. refdata.Client keys on the first.
	sector, ok := out["sector"].(map[string]any)
	if !ok {
		t.Fatalf("the body carries no sector object; refdata cannot tell \"no sector resolved\" "+
			"from \"this endpoint does not serve sectors\" without one: %v", out)
	}
	for _, k := range []string{"taxonomy", "code", "name"} {
		if _, present := sector[k]; !present {
			t.Errorf("sector.%s is absent — the taxonomy is load-bearing, because \"10\" is a "+
				"different industry in GICS and in ICB", k)
		}
	}
	if _, present := out["issuer_id"]; !present {
		t.Error("the body carries no issuer_id — the ISSUER concentration dimension has nothing " +
			"to resolve against (#640)")
	}
	if _, present := out["as_of"]; !present {
		t.Error("the body carries no as_of — without it no point-in-time question can be refused, " +
			"so a backtest silently gets today's classification")
	}
}

// The fields must carry the RESOLVED values, not merely exist. A key present
// and always empty is the same defect one layer in.
func TestSecurityEndpointCarriesTheResolvedClassification(t *testing.T) {
	feeds := []feed.VendorFeed{feed.SimFeed{
		Name: "BLOOMBERG",
		VRecords: []master.VendorRecord{{
			Vendor: "BLOOMBERG", InstrumentID: "AAPL", Priority: 0,
			AssetClass: "EQUITY", CurrencyCode: "USD",
			Sector:   master.Sector{Taxonomy: "GICS", Code: "45", Name: "Information Technology"},
			IssuerID: "LEI-APPLE",
			AsOf:     now,
		}},
	}}
	s := serverOver(t, feeds)
	out := readSecurity(t, s, "AAPL")

	sector := out["sector"].(map[string]any)
	if sector["taxonomy"] != "GICS" || sector["code"] != "45" {
		t.Errorf("sector = %v, want the resolved GICS/45", sector)
	}
	if out["issuer_id"] != "LEI-APPLE" {
		t.Errorf("issuer_id = %v, want LEI-APPLE", out["issuer_id"])
	}
	if out["as_of"] != now.UTC().Format("2006-01-02T15:04:05Z07:00") {
		t.Errorf("as_of = %v, want the record's own as_of in RFC3339", out["as_of"])
	}
}

// AN UNDATED RECORD SERVES AN EMPTY as_of, NOT "0001-01-01T00:00:00Z". A zero
// timestamp rendered as a real one reads as a 1st-century snapshot, and
// refdata's point-in-time rule would compare against it.
func TestAnUndatedRecordServesAnEmptyAsOf(t *testing.T) {
	feeds := []feed.VendorFeed{feed.SimFeed{
		Name: "BLOOMBERG",
		VRecords: []master.VendorRecord{{
			Vendor: "BLOOMBERG", InstrumentID: "AAPL", Priority: 0,
			AssetClass: "EQUITY", CurrencyCode: "USD",
		}},
	}}
	s := serverOver(t, feeds)
	out := readSecurity(t, s, "AAPL")

	if out["as_of"] != "" {
		t.Fatalf("as_of = %v for a record no vendor dated, want the empty string — a zero time "+
			"rendered as a real one is a classification claiming to be effective in the year 1",
			out["as_of"])
	}
}

// An instrument the master resolved with no sector and no issuer — FX, a broad
// index — answers with EMPTY values rather than 404 or an absent key. refdata
// returns ok=true for it, and compliance.unresolvedDimension is what turns the
// empty bucket into a refusal; collapsing the two here would report a
// reference-data gap that does not exist.
func TestASectorlessInstrumentAnswersWithEmptyValues(t *testing.T) {
	feeds := []feed.VendorFeed{feed.SimFeed{
		Name: "BLOOMBERG",
		VRecords: []master.VendorRecord{{
			Vendor: "BLOOMBERG", InstrumentID: "EURUSD", Priority: 0,
			AssetClass: "FX", CurrencyCode: "USD", AsOf: now,
		}},
	}}
	s := serverOver(t, feeds)
	out := readSecurity(t, s, "EURUSD")

	sector := out["sector"].(map[string]any)
	if sector["taxonomy"] != "" || sector["code"] != "" {
		t.Errorf("sector = %v, want empty strings for an instrument with no sector", sector)
	}
	if out["issuer_id"] != "" {
		t.Errorf("issuer_id = %v, want empty", out["issuer_id"])
	}
	if out["asset_class"] != "FX" {
		t.Errorf("asset_class = %v, want FX — the record IS resolved, it simply has no sector",
			out["asset_class"])
	}
}
