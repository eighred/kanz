package config

import (
	"log/slog"
	"os"
	"strings"
)

// Config is the optimization service runtime configuration, sourced from the
// environment (the same stateless reporting/compute shape as the performance
// service — OPT-01 is a construction/proposal plane). The service optimizes
// target weights and materializes a proposal's trades into OMS-01 commands; the
// concrete bus publisher + pre-trade gate are wired at the composition root
// behind the bridge seams (DEBT-02), so the default boot serves the
// optimize/propose + dry-run-materialize endpoints without a broker.
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
		Listen:       envOr("OPTIMIZATION_LISTEN", ":8080"),
		LogLevel:     parseLevel(os.Getenv("OPTIMIZATION_LOG_LEVEL")),
		OTLPEndpoint: os.Getenv("OPTIMIZATION_OTLP_ENDPOINT"),
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
