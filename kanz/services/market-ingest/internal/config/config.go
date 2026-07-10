// Package config loads market-ingest's runtime configuration from the
// environment. market-ingest is an outbound edge (it dials exchange depth feeds
// and publishes to the bus); it has no inbound trading API, so there is no
// bootstrap secrets file — only infra + the instrument set to track. Real venue
// depth sources bind at the composition root behind per-venue build tags; the
// default binary tracks the instruments with the vendor-free simulator.
package config

import (
	"log/slog"
	"os"
	"strings"
	"time"
)

// Config is the resolved configuration.
type Config struct {
	Listen       string // health/readiness listen address
	LogLevel     slog.Level
	OTLPEndpoint string

	NATSURL string
	Source  string

	// Instruments is the set of canonical instrument_ids to track (one book +
	// engine each). Empty ⇒ the service logs and idles (deny-by-default: it never
	// invents instruments).
	Instruments []string

	SnapshotInterval time.Duration
	SnapshotDepth    int
}

// Load reads and validates the environment.
func Load() Config {
	return Config{
		Listen:           envOr("MARKET_INGEST_LISTEN", ":8091"),
		LogLevel:         parseLevel(os.Getenv("MARKET_INGEST_LOG_LEVEL")),
		OTLPEndpoint:     os.Getenv("MARKET_INGEST_OTLP_ENDPOINT"),
		NATSURL:          envOr("MARKET_INGEST_NATS_URL", "nats://localhost:4222"),
		Source:           envOr("MARKET_INGEST_SOURCE", "market-ingest"),
		Instruments:      parseList(os.Getenv("MARKET_INGEST_INSTRUMENTS")),
		SnapshotInterval: parseDuration(os.Getenv("MARKET_INGEST_SNAPSHOT_INTERVAL"), time.Second),
		SnapshotDepth:    parseInt(os.Getenv("MARKET_INGEST_SNAPSHOT_DEPTH"), 20),
	}
}

func parseList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func parseInt(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return def
	}
	return n
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
