//go:build !okx

package main

import (
	"log/slog"

	"github.com/kanz-eng/kanz/pkg/alpha"
	"github.com/kanz-eng/kanz/services/market-ingest/internal/config"
)

// okxFeeds is a no-op when the OKX feeds are not compiled in.
func okxFeeds(config.Config, string, *slog.Logger) []alpha.Feed { return nil }
