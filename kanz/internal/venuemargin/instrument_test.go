package venuemargin

import (
	"context"
	"testing"

	"github.com/eighred/kanz/internal/execution"
)

// THE LIQUIDATION PRICE CARRIES THE INSTRUMENT IT IS ABOUT (#408 control 4).
//
// A liquidation price is only meaningful against a mark for the same
// instrument, and every consumer that holds marks is keyed by instrument_id.
// The exchange reports its OWN symbol, and the adapter's table is
// instrument -> symbol, so this is the one place both halves exist.

func TestALiquidationPriceCarriesItsInstrument(t *testing.T) {
	src := &fakeSource{obs: full()}
	pub := &capturePub{}
	r := reporterOver(src, pub)
	if err := r.Report(context.Background()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	msg := onlyState(t, pub)
	prices := msg.GetLiquidationPrices()
	if len(prices) != 1 {
		t.Fatalf("liquidation prices = %d, want 1", len(prices))
	}
	if got := prices[0].GetInstrumentId(); got != "BTC-USDT" {
		t.Errorf("instrument_id = %q, want BTC-USDT — without it nothing downstream can pair this "+
			"price with a mark", got)
	}
	// The venue's own spelling is kept: it is what the exchange said, and an
	// operator reconciling against the venue UI needs it.
	if got := prices[0].GetVenueSymbol(); got != "BTC-USDT-SWAP" {
		t.Errorf("venue_symbol = %q, want BTC-USDT-SWAP", got)
	}
}

// AN UNMAPPED SYMBOL IS PUBLISHED AND COUNTED UNCOVERED, not dropped.
//
// The fund holds a leveraged position in something this deployment cannot
// measure — worth an operator's attention — but no consumer can pair it with a
// mark, so coverage must not claim it as an input.
func TestAnUnmappedVenueSymbolIsPublishedAndCounted(t *testing.T) {
	src := &fakeSource{obs: full()}
	pub := &capturePub{}
	// A map that does not carry the position's symbol: the account trades
	// something this deployment does not list.
	r := reporterOver(src, pub, func(c *ReporterConfig) {
		c.Symbols = execution.StaticSymbolMap{"SOL-USDT": "SOL-USDT-SWAP"}
	})
	if err := r.Report(context.Background()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	msg := onlyState(t, pub)

	if len(msg.GetLiquidationPrices()) != 1 {
		t.Fatalf("the price was DROPPED; the fund holds a leveraged position nobody can now see")
	}
	if got := msg.GetLiquidationPrices()[0].GetInstrumentId(); got != "" {
		t.Errorf("instrument_id = %q, want empty — an unattributable symbol must not be guessed", got)
	}
	var named bool
	for _, e := range msg.GetCoverage().GetExclusions() {
		if e.GetInstrumentId() == "BTC-USDT-SWAP" && e.GetReason() == SkipUnmappedVenueSymbol {
			named = true
		}
	}
	if !named {
		t.Errorf("the unmapped symbol is not named in coverage: %v", msg.GetCoverage().GetExclusions())
	}
	// It must NOT be contributed: a consumer cannot use it, and coverage that
	// counted it would claim an input the measure has to skip.
	if msg.GetCoverage().GetExcludedCount() == 0 {
		t.Error("excluded_count is 0 with an unattributable price on the wire")
	}
}

// A NIL SYMBOL MAP DEGRADES EXACTLY ONE FIELD. The account's maintenance margin
// and margin ratio are the venue's own figures and need no attribution; only the
// per-position prices lose theirs, visibly.
func TestANilSymbolMapStillReportsTheAccountFigures(t *testing.T) {
	src := &fakeSource{obs: full()}
	pub := &capturePub{}
	r := reporterOver(src, pub, func(c *ReporterConfig) { c.Symbols = nil })
	if err := r.Report(context.Background()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	msg := onlyState(t, pub)
	if msg.GetMaintenanceMargin() == nil || msg.GetMarginRatio() == nil {
		t.Error("a nil symbol map lost the account-level figures, which need no attribution")
	}
	if got := msg.GetLiquidationPrices()[0].GetInstrumentId(); got != "" {
		t.Errorf("instrument_id = %q with no symbol map", got)
	}
}
