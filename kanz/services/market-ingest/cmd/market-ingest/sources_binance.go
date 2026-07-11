//go:build binance

package main

import (
	"log/slog"

	"github.com/kanz-eng/kanz/services/market-ingest/internal/config"
	"github.com/kanz-eng/kanz/services/market-ingest/internal/depth"
)

// binanceDepthSources (binance build) binds the live Binance Spot L2 depth
// websocket for an instrument, when a venue symbol is mapped for it. An unmapped
// instrument is not tradeable here and is skipped — the edge never invents a
// symbol. Depth is public market data: no API key is required.
func binanceDepthSources(cfg config.Config, instrument string, logger *slog.Logger) []venueSource {
	symbol, ok := cfg.BinanceSymbols[instrument]
	if !ok {
		return nil
	}
	logger.Info("binance depth source wired",
		"instrument", instrument, "symbol", symbol, "ws", cfg.BinanceWSBase)
	return []venueSource{{
		mic: cfg.BinanceMIC,
		src: depth.NewBinanceSource(depth.BinanceConfig{
			InstrumentID: instrument,
			Symbol:       symbol,
			MIC:          cfg.BinanceMIC,
			WSBase:       cfg.BinanceWSBase,
			RESTBase:     cfg.BinanceRESTBase,
			Limit:        cfg.DepthLimit,
			DNSTTL:       cfg.DNSTTL,
		}),
	}}
}
