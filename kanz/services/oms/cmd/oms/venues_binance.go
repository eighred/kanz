//go:build binance

package main

import (
	"log/slog"
	"os"
	"strings"

	"github.com/kanz-eng/kanz/services/oms/internal/config"
	"github.com/kanz-eng/kanz/services/oms/internal/execution"
)

// configuredVenues (binance build) routes to Binance Spot when credentials are
// present, falling back to the SimVenue otherwise so a tagged binary still runs
// in dev without live keys. Keys come from the environment / a mock Vault mount
// (never from code or the repo).
func configuredVenues(cfg config.Config, logger *slog.Logger) []execution.Venue {
	apiKey := secretEnv("BINANCE_API_KEY")
	apiSecret := secretEnv("BINANCE_API_SECRET")
	if apiKey == "" || apiSecret == "" {
		logger.Warn("binance: no API credentials — routing to SimVenue (set BINANCE_API_KEY/_SECRET for testnet)")
		return []execution.Venue{execution.NewSimVenue(cfg.SimVenueMIC)}
	}
	baseURL := envOr("BINANCE_BASE_URL", "https://testnet.binance.vision")
	venue := execution.NewBinanceVenueFromSettings(execution.VenueSettings{
		MIC:          envOr("BINANCE_MIC", "BINANCE"),
		BaseURL:      baseURL,
		APIKey:       apiKey,
		APISecret:    apiSecret,
		Symbols:      parseSymbolMap(os.Getenv("BINANCE_SYMBOLS")),
		WeightBudget: 1200,
		OnThrottle: func() {
			logger.Error("binance: REST weight budget exhausted — backing off (structural alert)")
		},
	})
	logger.Info("binance venue wired", "base_url", baseURL)
	return []execution.Venue{venue}
}

// parseSymbolMap parses "BTC-USD=BTCUSDT,ETH-USD=ETHUSDT" into a map.
func parseSymbolMap(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if k, v, ok := strings.Cut(pair, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// secretEnv prefers a CSI/Vault file mount (<k>_FILE) over a plaintext env var.
func secretEnv(k string) string {
	if p := os.Getenv(k + "_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv(k)
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
