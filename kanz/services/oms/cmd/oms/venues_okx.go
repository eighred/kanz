//go:build okx

package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/kanz-eng/kanz/services/oms/internal/config"
	"github.com/kanz-eng/kanz/services/oms/internal/execution"
)

// okxCloseRegistry is the shared in-flight-close registry the OKX healing
// watchdog drains (the In-Flight Certainty seam). The OMS venue-close dispatch
// path Tracks a close here when a cancel / IOC market close is sent to OKX;
// until that dispatch path lands it stays empty and the watchdog is a no-op —
// infrastructure ready, never fabricating a close.
var okxCloseRegistry = execution.NewCloseRegistry()

// okxVenues (okx build) builds the OKX Spot venue and starts its background
// workers (user-data stream, REST reconciliation, ticker feed) wired to the
// order store adapter + producer, when credentials are present. OKX needs an API
// passphrase in addition to the key/secret. Empty when unconfigured — the
// aggregator falls back to SimVenue. Keys come from env / a mock Vault mount,
// never from code. Mirrors the Binance connector for full multi-venue parity.
func okxVenues(ctx context.Context, cfg config.Config, adapter storeAdapter, producer execution.Publisher, logger *slog.Logger) []execution.Venue {
	apiKey := secretEnv("OKX_API_KEY")
	apiSecret := secretEnv("OKX_API_SECRET")
	passphrase := secretEnv("OKX_API_PASSPHRASE")
	if apiKey == "" || apiSecret == "" || passphrase == "" {
		logger.Warn("okx: no API credentials — venue not wired (set OKX_API_KEY/_SECRET/_PASSPHRASE)")
		return nil
	}
	baseURL := envOr("OKX_BASE_URL", "https://www.okx.com")
	wsURL := envOr("OKX_WS_URL", "wss://ws.okx.com:8443/ws/v5/private")
	conn := execution.NewOKXConnector(execution.VenueSettings{
		MIC:        envOr("OKX_MIC", "OKX"),
		BaseURL:    baseURL,
		APIKey:     apiKey,
		APISecret:  apiSecret,
		Passphrase: passphrase,
		Symbols:    parseSymbolMap(os.Getenv("OKX_SYMBOLS")),
		OnThrottle: func() {
			logger.Error("okx: REST request budget exhausted — backing off (structural alert)")
		},
	}, wsURL)
	conn.Start(ctx, execution.WorkerDeps{
		Publisher: producer, Lookup: adapter, Expected: adapter,
		Closes: okxCloseRegistry, Tenant: os.Getenv("OKX_TENANT"), Logger: logger,
	})
	logger.Info("okx venue wired", "base_url", baseURL, "ws_url", wsURL,
		"heal_timeout", "1500ms")
	return []execution.Venue{conn.Venue()}
}
