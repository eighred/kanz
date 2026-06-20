package config

import (
	"log/slog"
	"os"
	"strings"
)

// Config is the market-data service runtime configuration. Sourced from the
// environment so it composes with the CSI/Vault secret mounts (SEC-01d).
// Mirrors the risk-engine config shape.
type Config struct {
	Listen   string
	LogLevel slog.Level

	// NATSURL is the live spine (EVT-08) the service ingests market events
	// from. Empty ⇒ HTTP-only (probes up, no ingestion).
	NATSURL string
	// Source is the consumer identity (logging / future producer use).
	Source string
	// ConsumerGroup is the durable consumer name the service subscribes under.
	ConsumerGroup string
	// Subjects are the market subjects to ingest. Default is the NATS-spine
	// wildcard `market.>` (the MARKET stream); a Kafka-backed or narrowed
	// deployment lists concrete topics/subjects (e.g. only `market.equity.bar`
	// for an EOD-only price history). Comma-separated in the environment.
	Subjects []string

	// DatabaseURL is the Postgres/Timescale DSN for the durable price history
	// (MODEL-01b). Empty ⇒ the in-memory store: ingestion works but loses
	// history on restart (local/dev).
	DatabaseURL string

	// OTLPEndpoint is the OTel collector (host:port) for span export (OBS-01).
	OTLPEndpoint string
}

// DefaultSubjects is the ingestion subscription when none is configured.
var DefaultSubjects = []string{"market.>"}

func Load() (Config, error) {
	subjects := splitList(os.Getenv("MARKET_DATA_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = DefaultSubjects
	}
	return Config{
		Listen:        envOr("MARKET_DATA_LISTEN", ":8082"),
		LogLevel:      parseLevel(envOr("MARKET_DATA_LOG_LEVEL", "info")),
		NATSURL:       os.Getenv("MARKET_DATA_NATS_URL"),
		Source:        envOr("MARKET_DATA_SOURCE", "market-data"),
		ConsumerGroup: envOr("MARKET_DATA_CONSUMER_GROUP", "market-data"),
		Subjects:      subjects,
		DatabaseURL:   secret("MARKET_DATA_DATABASE_URL"),
		OTLPEndpoint:  os.Getenv("MARKET_DATA_OTLP_ENDPOINT"),
	}, nil
}

// splitList parses a comma-separated env value into a trimmed, non-empty slice;
// an empty or all-whitespace value yields nil.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// secret resolves a sensitive value, preferring a CSI/Vault file mount
// (SEC-01d: the path in <k>_FILE) over a plaintext <k> env var.
func secret(k string) string {
	if p := os.Getenv(k + "_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv(k)
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
