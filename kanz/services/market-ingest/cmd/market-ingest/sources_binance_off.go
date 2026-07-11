//go:build !binance

package main

import (
	"log/slog"

	"github.com/kanz-eng/kanz/services/market-ingest/internal/config"
)

// binanceDepthSources is a no-op when the Binance depth feed is not compiled in.
func binanceDepthSources(config.Config, string, *slog.Logger) []venueSource { return nil }
