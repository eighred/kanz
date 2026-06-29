package master

import (
	"testing"
	"time"
)

func day(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

func TestResolve_Survivorship(t *testing.T) {
	// BLOOMBERG (priority 0, most trusted) has currency + sector but no
	// description; REFINITIV (priority 1) has the description + a CUSIP.
	records := []VendorRecord{
		{
			Vendor: "REFINITIV", InstrumentID: "INST1", Priority: 1,
			Identifiers:  Identifiers{ISIN: "US0000001", CUSIP: "000000019"},
			CurrencyCode: "EUR", // less trusted — must lose to BLOOMBERG
			Description:  "Apple Inc Common Stock",
			AsOf:         day(2026, 1, 2),
		},
		{
			Vendor: "BLOOMBERG", InstrumentID: "INST1", Priority: 0,
			Identifiers:  Identifiers{ISIN: "US0000001", FIGI: "BBG000B9XRY4"},
			AssetClass:   "EQUITY",
			Sector:       Sector{Taxonomy: "GICS", Code: "45"},
			CurrencyCode: "USD",
			AsOf:         day(2026, 1, 1),
		},
	}
	sm, conflicts := Resolve(records)
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}
	if sm.CurrencyCode != "USD" {
		t.Errorf("currency = %q, want USD (most-trusted vendor wins)", sm.CurrencyCode)
	}
	if sm.Provenance["currency_code"] != "BLOOMBERG" {
		t.Errorf("currency provenance = %q, want BLOOMBERG", sm.Provenance["currency_code"])
	}
	// Description: BLOOMBERG has none, so it falls through to REFINITIV.
	if sm.Description != "Apple Inc Common Stock" || sm.Provenance["description"] != "REFINITIV" {
		t.Errorf("description = %q from %q, want REFINITIV's", sm.Description, sm.Provenance["description"])
	}
	// Identifiers are the union (FIGI from BBG, CUSIP from RIC vendor).
	if sm.Identifiers.FIGI != "BBG000B9XRY4" || sm.Identifiers.CUSIP != "000000019" {
		t.Errorf("merged identifiers = %+v", sm.Identifiers)
	}
	// as_of is the latest vendor as_of.
	if !sm.AsOf.Equal(day(2026, 1, 2)) {
		t.Errorf("as_of = %v, want 2026-01-02", sm.AsOf)
	}
}

func TestResolve_IdentifierConflict(t *testing.T) {
	records := []VendorRecord{
		{Vendor: "BLOOMBERG", InstrumentID: "INST1", Priority: 0, Identifiers: Identifiers{ISIN: "US0000001"}, AsOf: day(2026, 1, 1)},
		{Vendor: "REFINITIV", InstrumentID: "INST1", Priority: 1, Identifiers: Identifiers{ISIN: "US9999999"}, AsOf: day(2026, 1, 1)},
	}
	sm, conflicts := Resolve(records)
	if len(conflicts) != 1 || conflicts[0].Scheme != SchemeISIN {
		t.Fatalf("want 1 ISIN conflict, got %v", conflicts)
	}
	// The survivor is still the most-trusted vendor's value.
	if sm.Identifiers.ISIN != "US0000001" {
		t.Errorf("survivor ISIN = %q, want BLOOMBERG's US0000001", sm.Identifiers.ISIN)
	}
	// The conflict lists both distinct values, sorted.
	if len(conflicts[0].Values) != 2 || conflicts[0].Values[0] != "US0000001" {
		t.Errorf("conflict values = %v", conflicts[0].Values)
	}
}

func TestResolveAsOf_PointInTime(t *testing.T) {
	// A reclassification: EQUITY as of Jan 1, then FUND as of Feb 1.
	records := []VendorRecord{
		{Vendor: "BLOOMBERG", InstrumentID: "INST1", Priority: 0, AssetClass: "EQUITY", AsOf: day(2026, 1, 1)},
		{Vendor: "BLOOMBERG", InstrumentID: "INST1", Priority: 0, AssetClass: "FUND", AsOf: day(2026, 2, 1)},
	}
	// As-of mid-January sees only the EQUITY record.
	sm, _ := ResolveAsOf(records, day(2026, 1, 15))
	if sm.AssetClass != "EQUITY" {
		t.Errorf("as-of Jan 15 asset_class = %q, want EQUITY", sm.AssetClass)
	}
	// As-of now (zero) sees the latest — FUND wins on recency at equal priority.
	smNow, _ := ResolveAsOf(records, time.Time{})
	if smNow.AssetClass != "FUND" {
		t.Errorf("as-of now asset_class = %q, want FUND (recency tie-break)", smNow.AssetClass)
	}
}

func TestCrosswalk(t *testing.T) {
	records := []VendorRecord{
		{Vendor: "BLOOMBERG", InstrumentID: "INST1", Priority: 0, Identifiers: Identifiers{ISIN: "US0000001", FIGI: "BBG1"}, AsOf: day(2026, 1, 1)},
		{Vendor: "ICE", InstrumentID: "INST2", Priority: 0, Identifiers: Identifiers{CUSIP: "222222229"}, AsOf: day(2026, 1, 1)},
	}
	x, conflicts := BuildCrosswalk(records)
	if len(conflicts) != 0 {
		t.Fatalf("unexpected crosswalk conflicts: %v", conflicts)
	}
	if id, ok := x.Lookup(SchemeISIN, "US0000001"); !ok || id != "INST1" {
		t.Errorf("ISIN lookup = %q,%v want INST1", id, ok)
	}
	if id, ok := x.Lookup(SchemeFIGI, "BBG1"); !ok || id != "INST1" {
		t.Errorf("FIGI lookup = %q,%v want INST1", id, ok)
	}
	if id, ok := x.Lookup(SchemeCUSIP, "222222229"); !ok || id != "INST2" {
		t.Errorf("CUSIP lookup = %q,%v want INST2", id, ok)
	}
	if _, ok := x.Lookup(SchemeISIN, "NOPE"); ok {
		t.Errorf("unknown ISIN should not resolve")
	}
}

func TestCrosswalk_ConflictAcrossInstruments(t *testing.T) {
	// The same ISIN claimed by two different instruments — a crosswalk break.
	records := []VendorRecord{
		{Vendor: "BLOOMBERG", InstrumentID: "INST1", Priority: 0, Identifiers: Identifiers{ISIN: "US0000001"}, AsOf: day(2026, 1, 1)},
		{Vendor: "ICE", InstrumentID: "INST2", Priority: 0, Identifiers: Identifiers{ISIN: "US0000001"}, AsOf: day(2026, 1, 1)},
	}
	_, conflicts := BuildCrosswalk(records)
	if len(conflicts) != 1 {
		t.Fatalf("want 1 crosswalk conflict, got %v", conflicts)
	}
}

func TestResolve_Empty(t *testing.T) {
	sm, conflicts := Resolve(nil)
	if sm.InstrumentID != "" || conflicts != nil {
		t.Errorf("empty resolve = %+v, %v", sm, conflicts)
	}
}
