package main

import (
	"strings"
	"testing"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/services/venue-binance/internal/config"
)

// A SYMBOL MAP IS TWO CLAIMS SIDE BY SIDE (#407): a canonical id, and the symbol
// this adapter actually sends to the exchange. Only the second decides what is
// bought. The estate shipped "BTC-USD=BTCUSDT" — a USDT-quoted pair recorded as
// dollars — and no layer compared them, because after this point the symbol goes
// to the exchange and the id goes into the ledger and nothing sees both.
//
// These drive the SAME execution.ParseSymbolMap and the SAME StaticSymbolMap the
// composition root uses, so the check cannot pass here and be absent there.
func TestTheShippedMappingIsDetectedAsMisdescribing(t *testing.T) {
	symbols := mustSymbols(t, "BTC-USD=BTCUSDT,ETH-USD=ETHUSDT")

	bad := symbols.Mismatches()
	if len(bad) != 2 {
		t.Fatalf("Mismatches() = %d, want 2 — both shipped mappings name USD and trade USDT", len(bad))
	}
	for _, in := range bad {
		if in.Pair.Quote != "USDT" {
			t.Errorf("%s: traded quote = %q, want USDT", in.InstrumentID, in.Pair.Quote)
		}
		if !strings.HasSuffix(in.InstrumentID, "-USD") {
			t.Errorf("%s: expected an id claiming USD", in.InstrumentID)
		}
	}
}

// THE CORRECTED MAP IS SILENT, so BINANCE_REQUIRE_QUOTE_MATCH can be armed on a
// properly configured estate without refusing to start. Without this, the test
// above is satisfied by a check that flags everything.
func TestTheCorrectedMappingPasses(t *testing.T) {
	symbols := mustSymbols(t, "BTC-USDT=BTCUSDT,ETH-USDT=ETHUSDT")

	if bad := symbols.Mismatches(); len(bad) != 0 {
		t.Fatalf("Mismatches() = %+v, want none — this is the mapping the manifests now ship", bad)
	}
}

// The control is OFF by default, which is the same stance every other REQUIRE_
// on this platform takes: a control that refuses to start every adapter nobody
// has corrected yet is a trading outage, and it is armed WITH the estate in hand.
func TestQuoteMatchIsNotRequiredByDefault(t *testing.T) {
	t.Setenv("BINANCE_REQUIRE_QUOTE_MATCH", "")
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("config.Load needs more environment than this test provides: %v", err)
	}
	if cfg.RequireQuoteMatch {
		t.Error("RequireQuoteMatch defaults to true — a schema-level correctness check must not " +
			"become a startup outage for every deployment that has not been corrected yet")
	}
}

// mustSymbols parses through the production parser, so these fixtures are held
// to the same refusals a deployment is — a fixture the real parser would reject
// would otherwise assert behaviour no venue can reach.
func mustSymbols(t *testing.T, spec string) execution.StaticSymbolMap {
	t.Helper()
	m, err := execution.ParseSymbolMap(spec)
	if err != nil {
		t.Fatalf("ParseSymbolMap(%q): %v", spec, err)
	}
	return m
}
