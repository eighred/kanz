package main

import (
	"log/slog"

	"github.com/kanz-eng/kanz/pkg/alpha"
	"github.com/kanz-eng/kanz/services/market-ingest/internal/config"
)

// feeds is the market-ingest composition root's feed selector, mirroring the OMS
// venue aggregator: it collects whatever exchange feeds are compiled in — Binance
// under -tags binance, OKX under -tags okx, each contributed by a
// build-tag-split helper — and falls back to the vendor-free deterministic
// simulator when none are configured, so the default binary and dev builds still
// run end to end without a network.
//
// One feed per (instrument, venue): depth is per-venue and books are never merged
// across venues, so a build carrying both exchange tags folds TWO independent
// books for the same instrument. That is exactly the shape a cross-venue engine
// reads — it compares the venues itself.
func feeds(cfg config.Config, logger *slog.Logger) []alpha.Feed {
	var out []alpha.Feed
	for _, instrument := range cfg.Instruments {
		out = append(out, binanceFeeds(cfg, instrument, logger)...)
		out = append(out, okxFeeds(cfg, instrument, logger)...)
	}
	if len(out) == 0 && len(cfg.Instruments) > 0 {
		logger.Info("no exchange feeds configured — folding the deterministic simulator")
		for _, instrument := range cfg.Instruments {
			out = append(out, alpha.SimFeed(instrument))
		}
	}
	return out
}
