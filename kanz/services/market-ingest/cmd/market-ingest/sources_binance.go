package main

import (
	"log/slog"

	"github.com/eighred/kanz/pkg/alpha"
	"github.com/eighred/kanz/services/market-ingest/internal/config"
)

// binanceFeeds (binance build) binds the live Binance Spot depth + trade feeds for
// an instrument, when a venue symbol is mapped for it. An unmapped instrument is
// not tracked here — the edge never invents a symbol. Both streams are PUBLIC
// market data: no API key is required.
func binanceFeeds(cfg config.Config, instrument string, logger *slog.Logger) []alpha.Feed {
	symbol, ok := cfg.BinanceSymbols[instrument]
	if !ok {
		return nil
	}
	logger.Info("binance feed wired",
		"instrument", instrument, "symbol", symbol, "ws", cfg.BinanceWSBase)
	return []alpha.Feed{alpha.BinanceFeed(alpha.VenueFeedConfig{
		InstrumentID: instrument, Symbol: symbol, MIC: cfg.BinanceMIC,
		WSBase: cfg.BinanceWSBase, RESTBase: cfg.BinanceRESTBase,
		DepthLimit: cfg.DepthLimit, DNSTTL: cfg.DNSTTL,
	})}
}
