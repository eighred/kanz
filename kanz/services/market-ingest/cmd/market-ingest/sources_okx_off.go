//go:build !okx

package main

import (
	"log/slog"

	"github.com/kanz-eng/kanz/services/market-ingest/internal/config"
)

// okxDepthSources is a no-op when the OKX depth feed is not compiled in.
func okxDepthSources(config.Config, string, *slog.Logger) []venueSource { return nil }
