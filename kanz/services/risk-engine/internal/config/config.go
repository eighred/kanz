package config

import (
	"log/slog"
	"os"
	"strings"
)

// Config is the risk-engine runtime configuration. Sourced from the
// environment so it composes with the CSI/Vault secret mounts (SEC-01d).
// Durable-state knobs (Postgres) arrive with PERS-01.
type Config struct {
	Listen   string
	LogLevel slog.Level

	// NATSURL is the live spine (EVT-08). Empty ⇒ the service runs
	// HTTP-only (probes up, no ingestion) — useful for a bare scaffold
	// deploy or local boot without a broker.
	NATSURL string
	// Source is the producer identity stamped on emitted FACTs (EVT-17b).
	Source string
}

func Load() (Config, error) {
	return Config{
		Listen:   envOr("RISK_ENGINE_LISTEN", ":8081"),
		LogLevel: parseLevel(envOr("RISK_ENGINE_LOG_LEVEL", "info")),
		NATSURL:  os.Getenv("RISK_ENGINE_NATS_URL"),
		Source:   envOr("RISK_ENGINE_SOURCE", "risk-engine"),
	}, nil
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
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
