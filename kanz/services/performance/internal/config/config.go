package config

import (
	"log/slog"
	"os"
	"strings"
)

// Config is the performance service runtime configuration, sourced from the
// environment (same shape as the lineage/audit read services — PERF-01c is a
// reporting plane, not an event consumer). The service is a stateless analytics
// endpoint over internal/performance: it computes time-/money-weighted returns,
// benchmark-relative active return, Brinson attribution, and ex-post risk from
// request-supplied inputs, so it needs no broker or database to serve.
type Config struct {
	Listen   string
	LogLevel slog.Level

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ no
	// tracing exporter.
	OTLPEndpoint string
}

// Load reads the configuration from the environment with production-safe
// defaults.
func Load() (Config, error) {
	return Config{
		Listen:       envOr("PERFORMANCE_LISTEN", ":8080"),
		LogLevel:     parseLevel(os.Getenv("PERFORMANCE_LOG_LEVEL")),
		OTLPEndpoint: os.Getenv("PERFORMANCE_OTLP_ENDPOINT"),
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
