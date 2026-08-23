package master

import "testing"

// THE GOLDEN RECORD CARRIES AN ISSUER (#640).
//
// It did not, and that single missing field is why an issuer-concentration or
// issuer-exclusion mandate had nothing on this estate to resolve against: the
// master resolved sector and asset class and stopped, so
// reference.v1.InstrumentReference.issuer_id was never populated by anything
// and internal/refdata had nothing to serve the ISSUER dimension from.

func TestResolve_IssuerSurvivesByVendorTrust(t *testing.T) {
	records := []VendorRecord{
		{Vendor: "ICE", InstrumentID: "AAPL", Priority: 2, IssuerID: "ICE-APPLE", AsOf: day(2026, 1, 1)},
		{Vendor: "BLOOMBERG", InstrumentID: "AAPL", Priority: 0, IssuerID: "LEI-APPLE", AsOf: day(2026, 1, 1)},
		{Vendor: "REFINITIV", InstrumentID: "AAPL", Priority: 1, IssuerID: "RFN-APPLE", AsOf: day(2026, 1, 1)},
	}
	sm, _ := Resolve(records)

	// SAME RULE AS EVERY OTHER SCALAR: lowest priority wins. An issuer resolved
	// by a different rule from the sector beside it would let one golden record
	// describe two different companies.
	if sm.IssuerID != "LEI-APPLE" {
		t.Fatalf("IssuerID = %q, want LEI-APPLE (BLOOMBERG, priority 0)", sm.IssuerID)
	}
	if got := sm.Provenance["issuer_id"]; got != "BLOOMBERG" {
		t.Errorf("provenance[issuer_id] = %q, want BLOOMBERG — a concentration limit that fires "+
			"on an issuer must be able to say which vendor said so", got)
	}
}

// THE HIGHEST-TRUST VENDOR MAY NOT REPORT ONE. Survivorship takes the first
// NON-EMPTY value in trust order, so a primary vendor with no issuer column
// must fall through rather than resolving the field to "".
func TestResolve_IssuerFallsThroughAnEmptyPrimaryVendor(t *testing.T) {
	sm, _ := Resolve([]VendorRecord{
		{Vendor: "BLOOMBERG", InstrumentID: "AAPL", Priority: 0, AssetClass: "EQUITY", AsOf: day(2026, 1, 1)},
		{Vendor: "ICE", InstrumentID: "AAPL", Priority: 1, IssuerID: "LEI-APPLE", AsOf: day(2026, 1, 1)},
	})
	if sm.IssuerID != "LEI-APPLE" {
		t.Fatalf("IssuerID = %q, want LEI-APPLE from the fallback vendor", sm.IssuerID)
	}
	if got := sm.Provenance["issuer_id"]; got != "ICE" {
		t.Errorf("provenance[issuer_id] = %q, want ICE", got)
	}
}

// AN INSTRUMENT WITH NO ISSUER ANYWHERE RESOLVES TO EMPTY, AND SAYS NOTHING
// ABOUT PROVENANCE. Empty is a real answer here — FX and broad-index
// instruments have no issuing entity — and stamping a vendor against a field
// nobody reported would claim a source for a blank.
func TestResolve_NoVendorReportsAnIssuer(t *testing.T) {
	sm, _ := Resolve([]VendorRecord{
		{Vendor: "BLOOMBERG", InstrumentID: "EURUSD", Priority: 0, AssetClass: "FX", AsOf: day(2026, 1, 1)},
	})
	if sm.IssuerID != "" {
		t.Fatalf("IssuerID = %q, want empty", sm.IssuerID)
	}
	if _, claimed := sm.Provenance["issuer_id"]; claimed {
		t.Errorf("provenance claims a vendor for an issuer nobody reported: %v", sm.Provenance)
	}
}

// The point-in-time read must carry it too — a re-evaluation as of a past date
// resolves the issuer the vendors reported then, not today's.
func TestResolveAsOf_CarriesTheIssuer(t *testing.T) {
	records := []VendorRecord{
		{Vendor: "BLOOMBERG", InstrumentID: "AAPL", Priority: 0, IssuerID: "OLD-APPLE", AsOf: day(2026, 1, 1)},
		{Vendor: "BLOOMBERG", InstrumentID: "AAPL", Priority: 0, IssuerID: "NEW-APPLE", AsOf: day(2026, 6, 1)},
	}
	sm, _ := ResolveAsOf(records, day(2026, 3, 1))
	if sm.IssuerID != "OLD-APPLE" {
		t.Fatalf("IssuerID as of 2026-03-01 = %q, want OLD-APPLE — the later record was not yet "+
			"effective", sm.IssuerID)
	}
}
