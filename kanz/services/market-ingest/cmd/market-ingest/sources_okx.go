package main

import (
	"log/slog"

	"github.com/kanz-eng/kanz/pkg/alpha"
	"github.com/kanz-eng/kanz/services/market-ingest/internal/config"
)

// okxFeeds (okx build) binds the live OKX v5 public books + trades feeds for an
// instrument, when a venue instId is mapped for it. An unmapped instrument is not
// tracked here — the edge never invents a symbol. Both streams are PUBLIC market
// data: no API key is required.
func okxFeeds(cfg config.Config, instrument string, logger *slog.Logger) []alpha.Feed {
	instID, ok := cfg.OKXSymbols[instrument]
	if !ok {
		return nil
	}
	logger.Info("okx feed wired",
		"instrument", instrument, "inst_id", instID, "ws", cfg.OKXWSURL)
	return []alpha.Feed{alpha.OKXFeed(alpha.VenueFeedConfig{
		InstrumentID: instrument, Symbol: instID, MIC: cfg.OKXMIC,
		WSBase: cfg.OKXWSURL, DNSTTL: cfg.DNSTTL,
	})}
}
