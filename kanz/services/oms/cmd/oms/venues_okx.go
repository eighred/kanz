//go:build okx

package main

import (
	"log/slog"
	"os"

	"github.com/kanz-eng/kanz/services/oms/internal/config"
	"github.com/kanz-eng/kanz/services/oms/internal/execution"
)

// okxVenues (okx build) builds the OKX Spot venue when credentials are present.
// OKX needs an API passphrase in addition to the key/secret. Empty when
// unconfigured. Its background workers (user-data, reconciliation, ticker)
// mirror the Binance connector and follow in a later slice; M4 delivers the
// execution seam so the allocation matrix can route to two venues.
func okxVenues(_ config.Config, logger *slog.Logger) []execution.Venue {
	apiKey := secretEnv("OKX_API_KEY")
	apiSecret := secretEnv("OKX_API_SECRET")
	passphrase := secretEnv("OKX_API_PASSPHRASE")
	if apiKey == "" || apiSecret == "" || passphrase == "" {
		logger.Warn("okx: no API credentials — venue not wired (set OKX_API_KEY/_SECRET/_PASSPHRASE)")
		return nil
	}
	baseURL := envOr("OKX_BASE_URL", "https://www.okx.com")
	venue := execution.NewOKXVenueFromSettings(execution.VenueSettings{
		MIC:        envOr("OKX_MIC", "OKX"),
		BaseURL:    baseURL,
		APIKey:     apiKey,
		APISecret:  apiSecret,
		Passphrase: passphrase,
		Symbols:    parseSymbolMap(os.Getenv("OKX_SYMBOLS")),
		OnThrottle: func() {
			logger.Error("okx: REST request budget exhausted — backing off (structural alert)")
		},
	})
	logger.Info("okx venue wired", "base_url", baseURL)
	return []execution.Venue{venue}
}
