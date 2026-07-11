package main

import (
	"log/slog"

	"github.com/kanz-eng/kanz/services/market-ingest/internal/config"
	"github.com/kanz-eng/kanz/services/market-ingest/internal/depth"
)

// venueSource is one instrument's depth feed on one venue. Depth is per-venue and
// books are never merged across venues (see market.v1.OrderBookSnapshot), so a
// build carrying both exchange tags tracks TWO books for the same instrument —
// one per venue. That is exactly the shape the cross-venue engines read.
type venueSource struct {
	mic string
	src depth.DepthSource
}

// depthSources is the market-ingest composition root's feed selector, mirroring
// the OMS venue aggregator: it collects whatever exchange depth feeds are
// compiled in — Binance under -tags binance, OKX under -tags okx, each
// contributed by a build-tag-split helper — and falls back to the vendor-free
// deterministic simulator when none are configured, so the default binary and dev
// builds still run end to end without a network.
func depthSources(cfg config.Config, instrument string, logger *slog.Logger) []venueSource {
	var out []venueSource
	out = append(out, binanceDepthSources(cfg, instrument, logger)...)
	out = append(out, okxDepthSources(cfg, instrument, logger)...)
	if len(out) == 0 {
		return []venueSource{{
			mic: "SIM",
			src: depth.NewSimSource(depth.SimConfig{InstrumentID: instrument, Symbol: instrument}),
		}}
	}
	return out
}
