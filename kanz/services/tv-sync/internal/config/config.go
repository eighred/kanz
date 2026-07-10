// Package config loads tv-sync's runtime configuration from the environment.
// tv-sync is a pure projection — it needs only where the bus is and where to
// serve the Broker API.
package config

import (
	"log/slog"
	"os"
	"strings"
)

// Config is the resolved configuration.
type Config struct {
	Listen        string
	LogLevel      slog.Level
	OTLPEndpoint  string
	NATSURL       string
	Source        string
	ConsumerGroup string
	// PriceSubject is the market price spine tv-sync folds into its MarkSource
	// for live unrealized P&L (M3.5). Default "market.>" catches every market
	// variant, incl. the Binance ticker feed's market.crypto.trade.
	PriceSubject string
}

// Load reads TV_SYNC_* environment variables with production-safe defaults.
func Load() (Config, error) {
	return Config{
		Listen:        envOr("TV_SYNC_LISTEN", ":8091"),
		LogLevel:      parseLevel(os.Getenv("TV_SYNC_LOG_LEVEL")),
		OTLPEndpoint:  os.Getenv("TV_SYNC_OTLP_ENDPOINT"),
		NATSURL:       envOr("TV_SYNC_NATS_URL", "nats://localhost:4222"),
		Source:        envOr("TV_SYNC_SOURCE", "tv-sync"),
		ConsumerGroup: envOr("TV_SYNC_CONSUMER_GROUP", "tv-sync"),
		PriceSubject:  envOr("TV_SYNC_PRICE_SUBJECT", "market.>"),
	}, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
