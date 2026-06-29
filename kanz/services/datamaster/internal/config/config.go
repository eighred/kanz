package config

import (
	"log/slog"
	"os"
	"strings"
)

// Config is the datamaster (golden-source) service runtime configuration, sourced
// from the environment. The service resolves a golden security master across
// vendor feeds and arbitrates multi-source prices with an exception queue
// (MASTER-01). Vendor feeds default to the dependency-free SimFeed (a real
// Bloomberg/Refinitiv/ICE adapter plugs in behind the feed.VendorFeed seam at the
// composition root); the in-memory stores serve the read endpoints without an
// external dependency.
type Config struct {
	Listen   string
	LogLevel slog.Level

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string
}

// Load reads the configuration from the environment with production-safe
// defaults.
func Load() (Config, error) {
	return Config{
		Listen:       envOr("DATAMASTER_LISTEN", ":8080"),
		LogLevel:     parseLevel(os.Getenv("DATAMASTER_LOG_LEVEL")),
		OTLPEndpoint: os.Getenv("DATAMASTER_OTLP_ENDPOINT"),
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
