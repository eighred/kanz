//go:build okx

package main

import (
	"log/slog"

	"github.com/kanz-eng/kanz/services/market-ingest/internal/config"
	"github.com/kanz-eng/kanz/services/market-ingest/internal/depth"
)

// okxDepthSources (okx build) binds the live OKX v5 public `books` websocket for
// an instrument, when a venue instId is mapped for it. An unmapped instrument is
// skipped — the edge never invents a symbol. Depth is public market data: no API
// key is required.
func okxDepthSources(cfg config.Config, instrument string, logger *slog.Logger) []venueSource {
	instID, ok := cfg.OKXSymbols[instrument]
	if !ok {
		return nil
	}
	logger.Info("okx depth source wired",
		"instrument", instrument, "inst_id", instID, "ws", cfg.OKXWSURL)
	return []venueSource{{
		mic: cfg.OKXMIC,
		src: depth.NewOKXSource(depth.OKXConfig{
			InstrumentID: instrument,
			InstID:       instID,
			MIC:          cfg.OKXMIC,
			WSURL:        cfg.OKXWSURL,
			DNSTTL:       cfg.DNSTTL,
		}),
	}}
}
