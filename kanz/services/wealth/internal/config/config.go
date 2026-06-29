package config

import (
	"log/slog"
	"os"
	"strings"
)

// Config is the wealth (advisory) service runtime configuration, sourced from the
// environment. The service aggregates a household's accounts into a virtual
// portfolio and serves the household-level exposure view (WEALTH-01b). The
// household store defaults to in-memory (a durable backend plugs in behind the
// Store seam at the composition root, the PERS-01 stance); the bus consumer that
// feeds household/account/holding state is wired there too, so the default boot
// serves the read endpoints without a broker.
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
		Listen:       envOr("WEALTH_LISTEN", ":8080"),
		LogLevel:     parseLevel(os.Getenv("WEALTH_LOG_LEVEL")),
		OTLPEndpoint: os.Getenv("WEALTH_OTLP_ENDPOINT"),
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
