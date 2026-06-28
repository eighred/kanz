package config

import (
	"log/slog"
	"os"
	"strings"
)

// Config is the alternatives (private-markets) service runtime configuration,
// sourced from the environment. The service folds commitment lifecycle events
// (capital calls, distributions, NAV marks) into an event-sourced fund position
// and serves position summaries + private-asset metrics (IRR/TVPI/DPI/RVPI). The
// journal store defaults to in-memory (a durable backend plugs in behind
// fund.Store at the composition root, the PERS-01 stance); the bus consumer that
// feeds the journal is wired there too, so the default boot serves the read
// endpoints without a broker.
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
		Listen:       envOr("ALTERNATIVES_LISTEN", ":8080"),
		LogLevel:     parseLevel(os.Getenv("ALTERNATIVES_LOG_LEVEL")),
		OTLPEndpoint: os.Getenv("ALTERNATIVES_OTLP_ENDPOINT"),
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
