package feed

import (
	"context"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/services/datamaster/internal/master"
)

type fakeRefSource struct {
	vendor   string
	priority int
	rows     []RefRow
	err      error
}

func (f fakeRefSource) Vendor() string                          { return f.vendor }
func (f fakeRefSource) Priority() int                           { return f.priority }
func (f fakeRefSource) Fetch(context.Context) ([]RefRow, error) { return f.rows, f.err }

func TestReferenceAdapter_NormalizesToVendorRecords(t *testing.T) {
	src := fakeRefSource{vendor: "BLOOMBERG", priority: 0, rows: []RefRow{
		{
			Symbol: "AAPL US Equity", ISIN: "US0378331005", FIGI: "BBG000B9XRY4", BloombergTicker: "AAPL US Equity",
			AssetClass: "EQUITY", SectorTaxonomy: "GICS", SectorCode: "4520", Currency: "USD",
			Description: "Apple Inc", AsOf: time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
		},
	}}
	a := NewReferenceAdapter(src, nil)
	if a.Vendor() != "BLOOMBERG" {
		t.Fatalf("vendor = %q", a.Vendor())
	}
	recs, err := a.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.InstrumentID != "BBG000B9XRY4" { // DefaultIDResolver prefers FIGI
		t.Errorf("instrument_id = %q, want the FIGI", r.InstrumentID)
	}
	if r.Vendor != "BLOOMBERG" || r.Priority != 0 {
		t.Errorf("vendor/priority = %q/%d", r.Vendor, r.Priority)
	}
	if r.Identifiers.ISIN != "US0378331005" || r.AssetClass != "EQUITY" || r.Sector.Code != "4520" {
		t.Errorf("identifiers/classification not normalized: %+v", r)
	}

	// Prices is nil for a reference feed.
	if p, _ := a.Prices(context.Background()); p != nil {
		t.Errorf("reference adapter should carry no prices, got %v", p)
	}

	// The normalized records feed the MASTER crosswalk + survivorship.
	x, conflicts := master.BuildCrosswalk(recs)
	if len(conflicts) != 0 {
		t.Fatalf("unexpected crosswalk conflicts: %v", conflicts)
	}
	if id, ok := x.Lookup(master.SchemeFIGI, "BBG000B9XRY4"); !ok || id != "BBG000B9XRY4" {
		t.Errorf("FIGI crosswalk = %q,%v", id, ok)
	}
}

func TestDefaultIDResolver_Precedence(t *testing.T) {
	// FIGI wins over ISIN; ISIN over CUSIP; Symbol is the last resort.
	if got := DefaultIDResolver(RefRow{Symbol: "X", CUSIP: "037833100", ISIN: "US0378331005", FIGI: "BBG1"}); got != "BBG1" {
		t.Errorf("got %q, want FIGI", got)
	}
	if got := DefaultIDResolver(RefRow{Symbol: "X", CUSIP: "037833100", ISIN: "US0378331005"}); got != "US0378331005" {
		t.Errorf("got %q, want ISIN", got)
	}
	if got := DefaultIDResolver(RefRow{Symbol: "X"}); got != "X" {
		t.Errorf("got %q, want Symbol fallback", got)
	}
}

func TestReferenceAdapter_SkipsUnresolvable(t *testing.T) {
	// A row with no identifiers at all (no id) is skipped — cannot be mastered.
	src := fakeRefSource{vendor: "ICE", rows: []RefRow{{AssetClass: "EQUITY"}, {Symbol: "OK", AssetClass: "EQUITY"}}}
	recs, err := NewReferenceAdapter(src, nil).Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].InstrumentID != "OK" {
		t.Errorf("expected only the resolvable row, got %+v", recs)
	}
}
