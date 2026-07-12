package feed

import (
	"context"
	"testing"
	"time"

	referencepb "github.com/kanz-eng/kanz-schemas-go/reference/v1"

	"github.com/kanz-eng/kanz/internal/dec"
	"github.com/kanz-eng/kanz/services/datamaster/internal/master"
	"github.com/kanz-eng/kanz/services/datamaster/internal/pricing"
)

func TestSimFeed(t *testing.T) {
	f := SimFeed{
		Name:     "BLOOMBERG",
		VRecords: []master.VendorRecord{{Vendor: "BLOOMBERG", InstrumentID: "INST1"}},
		Candidates: []pricing.Candidate{
			{Source: "BLOOMBERG", Price: dec.Rat("100"), AsOf: time.Unix(0, 0)},
		},
	}
	if f.Vendor() != "BLOOMBERG" {
		t.Errorf("vendor = %q", f.Vendor())
	}
	recs, _ := f.Records(context.Background())
	ps, _ := f.Prices(context.Background())
	if len(recs) != 1 || len(ps) != 1 {
		t.Fatalf("sim feed returned %d records, %d prices", len(recs), len(ps))
	}
	// Satisfies the VendorFeed interface.
	var _ VendorFeed = f
}

func TestNormalizeReference(t *testing.T) {
	sm := master.SecurityMaster{
		InstrumentID: "INST1",
		Identifiers:  master.Identifiers{ISIN: "US0000001", FIGI: "BBG1"},
		AssetClass:   "EQUITY",
		Sector:       master.Sector{Taxonomy: "GICS", Code: "45", Name: "Information Technology"},
		CurrencyCode: "USD",
		Description:  "Apple Inc",
		AsOf:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	ref := NormalizeReference(sm)
	if ref.GetInstrumentId() != "INST1" {
		t.Errorf("instrument_id = %q", ref.GetInstrumentId())
	}
	if ref.GetAssetClass() != referencepb.AssetClass_ASSET_CLASS_EQUITY {
		t.Errorf("asset_class = %v, want EQUITY", ref.GetAssetClass())
	}
	if ref.GetIdentifiers().GetIsin() != "US0000001" || ref.GetIdentifiers().GetFigi() != "BBG1" {
		t.Errorf("identifiers = %+v", ref.GetIdentifiers())
	}
	if ref.GetSector().GetCode() != "45" {
		t.Errorf("sector = %+v", ref.GetSector())
	}
	if ref.GetAsOf() == nil {
		t.Errorf("as_of not set")
	}
}

func TestNormalizeReference_UnknownAssetClass(t *testing.T) {
	ref := NormalizeReference(master.SecurityMaster{InstrumentID: "X", AssetClass: "WIDGET"})
	if ref.GetAssetClass() != referencepb.AssetClass_ASSET_CLASS_UNSPECIFIED {
		t.Errorf("unknown asset class should map to UNSPECIFIED, got %v", ref.GetAssetClass())
	}
}

func TestNormalizePrice(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	a := pricing.Arbitrate("INST1", []pricing.Candidate{{Source: "BLOOMBERG", Price: dec.Rat("123.45"), AsOf: now}}, nil, 0, now)
	ev := NormalizePrice(a, now)
	if ev == nil {
		t.Fatal("expected a quote event")
	}
	q := ev.GetQuote()
	if q == nil {
		t.Fatal("expected a quote payload")
	}
	// 123.45 at the platform's fixed wire scale (dec.ToProto, 8dp) ⇒ 12345000000e-8.
	if q.GetBidPrice().GetCoefficient() != 12_345_000_000 || q.GetBidPrice().GetExponent() != -8 {
		t.Errorf("bid = %+v, want 12345000000e-8", q.GetBidPrice())
	}
	if q.GetBidPrice().GetCoefficient() != q.GetAskPrice().GetCoefficient() {
		t.Errorf("bid != ask for a consensus mark")
	}
	// No consensus ⇒ nil event.
	if NormalizePrice(pricing.Arbitrate("X", nil, nil, 0, now), now) != nil {
		t.Errorf("missing price should normalize to nil")
	}
}
