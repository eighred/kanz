package main

// The feed selector's job is to bind REAL exchange feeds. Its other job — the one
// worth a test — is to refuse to invent prices when it cannot.
//
// SimFeed does not read a market, it GENERATES prices, and those prices publish to
// the bus as market.v1 FACTs that risk, NAV and the OMS's pricing all mark
// against. It used to be a SILENT fallback: no exchange feed compiled in ⇒ swap in
// the simulator, log at Info, report healthy. That is "inject data" wearing a
// healthy dashboard, and it is exactly what the never-fabricate rule forbids.

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/kanz-eng/kanz/services/market-ingest/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestFeedsRefusesToSimulateWhenNoSymbolsAreMapped(t *testing.T) {
	// Instruments requested, but no venue symbol mapped for any of them. The old
	// behaviour published generated prices. The correct behaviour is to refuse.
	cfg := config.Config{Instruments: []string{"BTC-USD"}}

	got, err := feeds(cfg, quietLogger())
	if err == nil {
		t.Fatal("feeds() silently substituted simulated prices for a real market feed — that is fabricated data on the bus")
	}
	if len(got) != 0 {
		t.Fatalf("feeds() returned %d feeds alongside the error", len(got))
	}
	if !strings.Contains(err.Error(), "SIMULATED") {
		t.Fatalf("the error must say plainly what it is refusing to do; got: %v", err)
	}
}

func TestFeedsSimulatesOnlyWhenExplicitlyAllowed(t *testing.T) {
	// Opt-in is still possible — for tests and local dev — but it must be asked
	// for, never arrived at by forgetting to configure a symbol map.
	cfg := config.Config{Instruments: []string{"BTC-USD"}, AllowSim: true}

	got, err := feeds(cfg, quietLogger())
	if err != nil {
		t.Fatalf("feeds() with AllowSim: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d feeds, want 1 sim feed", len(got))
	}
	if got[0].MIC != "SIM" {
		t.Fatalf("simulated feed is stamped MIC %q, want SIM — downstream must be able to tell it apart", got[0].MIC)
	}
}

func TestFeedsBindsTheRealVenueWhenASymbolIsMapped(t *testing.T) {
	// The exchange feeds are compiled in unconditionally now (no build tags). What
	// selects them is the symbol map — the runtime gate that was always there.
	cfg := config.Config{
		Instruments:    []string{"BTC-USD"},
		BinanceSymbols: map[string]string{"BTC-USD": "BTCUSDT"},
		BinanceMIC:     "BINANCE",
	}

	got, err := feeds(cfg, quietLogger())
	if err != nil {
		t.Fatalf("feeds(): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d feeds, want 1", len(got))
	}
	if got[0].MIC == "SIM" {
		t.Fatal("a mapped instrument bound the SIMULATOR instead of the live venue")
	}
	if got[0].InstrumentID != "BTC-USD" {
		t.Fatalf("feed instrument = %q", got[0].InstrumentID)
	}
}

func TestFeedsWithNoInstrumentsIsNotAnError(t *testing.T) {
	// Nothing asked for, nothing ingested, nothing invented.
	got, err := feeds(config.Config{}, quietLogger())
	if err != nil {
		t.Fatalf("feeds() with no instruments: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d feeds, want 0", len(got))
	}
}
