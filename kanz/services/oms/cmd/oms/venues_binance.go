//go:build binance

package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/kanz-eng/kanz/services/oms/internal/config"
	"github.com/kanz-eng/kanz/services/oms/internal/execution"
)

// binanceVenues (binance build) builds the Binance Spot venue and starts its
// M3.6 background workers (user-data stream, reconciliation, ticker feed) wired
// to the order store adapter + producer, when credentials are present. Empty
// when unconfigured — the aggregator falls back to SimVenue. Keys come from
// env / a mock Vault mount, never from code.
func binanceVenues(ctx context.Context, cfg config.Config, adapter storeAdapter, producer execution.Publisher, logger *slog.Logger) []execution.Venue {
	apiKey := secretEnv("BINANCE_API_KEY")
	apiSecret := secretEnv("BINANCE_API_SECRET")
	if apiKey == "" || apiSecret == "" {
		logger.Warn("binance: no API credentials — venue not wired (set BINANCE_API_KEY/_SECRET)")
		return nil
	}
	baseURL := envOr("BINANCE_BASE_URL", "https://testnet.binance.vision")
	wsBase := envOr("BINANCE_WS_BASE", "wss://testnet.binance.vision")
	conn := execution.NewBinanceConnector(execution.VenueSettings{
		MIC:          envOr("BINANCE_MIC", "BINANCE"),
		BaseURL:      baseURL,
		APIKey:       apiKey,
		APISecret:    apiSecret,
		Symbols:      parseSymbolMap(os.Getenv("BINANCE_SYMBOLS")),
		WeightBudget: 1200,
		OnThrottle: func() {
			logger.Error("binance: REST weight budget exhausted — backing off (structural alert)")
		},
	}, wsBase)
	conn.Start(ctx, execution.WorkerDeps{
		Publisher: producer, Lookup: adapter, Expected: adapter,
		Closes: closeRegistry, Tenant: os.Getenv("BINANCE_TENANT"), Logger: logger,
	})
	logger.Info("binance venue wired", "base_url", baseURL, "ws_base", wsBase,
		"heal_timeout", "1500ms")
	return []execution.Venue{conn.Venue()}
}
