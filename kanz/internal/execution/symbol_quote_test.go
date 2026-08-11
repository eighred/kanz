package execution

import "testing"

// THE MAPPING THE ESTATE ACTUALLY SHIPPED (#407): canonical "BTC-USD" pointing at
// Binance's BTCUSDT. Every position taken through it was USDT-quoted and recorded
// as dollars, and no layer compared the two — the id went to the ledger, the
// symbol went to the exchange, and nothing saw both.
//
// The pair is resolved from the SYMBOL, because the symbol is what is actually
// sent to the exchange. Resolving from the id would restate the same claim in two
// more fields and prove nothing.
func TestInstrumentsResolveTheQuoteFromTheVenueSymbol(t *testing.T) {
	m := StaticSymbolMap{
		"BTC-USD":  "BTCUSDT", // the shipped lie
		"ETH-USDT": "ETHUSDT", // corrected
		"SOL-USD":  "XSOLUSD", // a symbol this platform cannot decompose
	}

	got := map[string]InstrumentSymbol{}
	for _, in := range m.Instruments() {
		got[in.InstrumentID] = in
	}

	btc := got["BTC-USD"]
	if btc.Pair.Quote != "USDT" {
		t.Errorf("BTC-USD quote = %q, want USDT — the venue decides what is bought", btc.Pair.Quote)
	}
	if !btc.QuoteMismatch {
		t.Error("BTC-USD=BTCUSDT is not reported as a mismatch; this is the exact mapping that " +
			"records a stablecoin position as dollars")
	}

	eth := got["ETH-USDT"]
	if eth.QuoteMismatch {
		t.Error("a correctly named pair is reported as mismatched — arming the check would refuse every venue")
	}
	if eth.Pair.Quote != "USDT" {
		t.Errorf("ETH-USDT quote = %q, want USDT", eth.Pair.Quote)
	}

	// UNREADABLE IS NOT MISMATCHED. A venue with its own ticker for the asset must
	// not be refused at startup because of a string-handling opinion.
	sol := got["SOL-USD"]
	if sol.QuoteMismatch {
		t.Error("a symbol that could not be decomposed was reported as a mismatch — this refuses a working venue")
	}
}

// Mismatches is what an adapter refuses on, so it must name exactly the bad
// entries and nothing else.
func TestMismatchesNamesOnlyTheContradictoryEntries(t *testing.T) {
	m := StaticSymbolMap{
		"BTC-USD":  "BTCUSDT",
		"ETH-USDT": "ETHUSDT",
		"SOL-USD":  "XSOLUSD",
	}

	bad := m.Mismatches()
	if len(bad) != 1 {
		t.Fatalf("Mismatches() = %+v, want exactly BTC-USD", bad)
	}
	if bad[0].InstrumentID != "BTC-USD" {
		t.Errorf("Mismatches() named %q", bad[0].InstrumentID)
	}
	// Both sides must be nameable from the entry alone: an operator reading the
	// startup log has to see what was claimed AND what is traded.
	if bad[0].Pair.Base != "BTC" || bad[0].Pair.Quote != "USDT" || bad[0].VenueSymbol != "BTCUSDT" {
		t.Errorf("entry = %+v, want base BTC, traded quote USDT, symbol BTCUSDT", bad[0])
	}
}

// A CORRECT MAP IS SILENT, so arming the requirement is not a self-inflicted
// outage on a properly configured estate.
func TestACorrectSymbolMapHasNoMismatches(t *testing.T) {
	m := StaticSymbolMap{"BTC-USDT": "BTCUSDT", "ETH-USDT": "ETH-USDT"}
	if bad := m.Mismatches(); len(bad) != 0 {
		t.Fatalf("Mismatches() = %+v, want none", bad)
	}
}
