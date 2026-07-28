package main

import (
	"fmt"
	"log/slog"

	"github.com/eighred/kanz/pkg/alpha"
	"github.com/eighred/kanz/services/market-ingest/internal/config"
)

// feeds is the market-ingest composition root's feed selector. It binds the live
// exchange feeds for every instrument that has a venue symbol mapped.
//
// There are no build tags any more (INFRA-M7a-4). The Binance and OKX feeds are
// always compiled in — they are PUBLIC market data over a plain websocket, no API
// key, no vendor SDK — and which ones actually run is decided at RUNTIME by the
// symbol maps, exactly as it always was. The tag was redundant with a config gate
// that already existed, and it was hiding the fallback below.
//
// One feed per (instrument, venue): depth is per-venue and books are never merged
// across venues, so an instrument mapped on both folds TWO independent books. That
// is the shape a cross-venue engine reads — it compares the venues itself.
//
// THE SIMULATOR IS NOT A FALLBACK. It used to be: if nothing was compiled in, this
// function quietly swapped in alpha.SimFeed and logged it at Info. SimFeed does not
// read a market, it GENERATES prices — and those prices publish to the bus as
// market.v1 FACTs that risk, NAV, and pricing all mark against. A service that
// invents prices and reports itself healthy is the market-data twin of an OMS that
// simulates its fills, and it is the same rule broken: never inject data.
//
// So it is now an explicit, deliberate opt-in (MARKET_INGEST_ALLOW_SIM=true) and
// an error otherwise. An ingest with no mapped symbols publishes NOTHING, loudly,
// rather than publishing fiction quietly.
func feeds(cfg config.Config, logger *slog.Logger) ([]alpha.Feed, error) {
	var out []alpha.Feed
	for _, instrument := range cfg.Instruments {
		out = append(out, binanceFeeds(cfg, instrument, logger)...)
		out = append(out, okxFeeds(cfg, instrument, logger)...)
	}
	if len(out) > 0 || len(cfg.Instruments) == 0 {
		return out, nil
	}

	if !cfg.AllowSim {
		return nil, fmt.Errorf(
			"market-ingest: %d instrument(s) requested but no venue symbols mapped — "+
				"refusing to publish SIMULATED prices as market data. "+
				"Set MARKET_INGEST_BINANCE_SYMBOLS / _OKX_SYMBOLS, or MARKET_INGEST_ALLOW_SIM=true "+
				"if synthetic prices are genuinely what you want",
			len(cfg.Instruments))
	}
	// Opt-in, and it still says exactly what it is doing.
	logger.Warn("PUBLISHING SIMULATED MARKET DATA — no exchange feed is mapped. Every price on the bus is GENERATED, not observed; risk, NAV and pricing will all mark against fiction",
		"instruments", len(cfg.Instruments))
	for _, instrument := range cfg.Instruments {
		out = append(out, alpha.SimFeed(instrument))
	}
	return out, nil
}
