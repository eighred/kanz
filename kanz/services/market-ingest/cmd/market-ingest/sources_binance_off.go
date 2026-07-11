//go:build !binance

package main

import (
	"log/slog"

	"github.com/kanz-eng/kanz/pkg/alpha"
	"github.com/kanz-eng/kanz/services/market-ingest/internal/config"
)

// binanceFeeds is a no-op when the Binance feeds are not compiled in.
func binanceFeeds(config.Config, string, *slog.Logger) []alpha.Feed { return nil }
