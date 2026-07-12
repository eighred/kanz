package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the datamaster (golden-source) service runtime configuration, sourced
// from the environment. The service resolves a golden security master across
// vendor feeds and arbitrates multi-source prices with an exception queue
// (MASTER-01). A real Bloomberg/Refinitiv/ICE adapter plugs in behind the
// feed.RefSource seam at the composition root.
type Config struct {
	Listen   string
	LogLevel slog.Level

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string

	// DatabaseURL selects the durable golden store and exception queue. Empty ⇒
	// the in-memory stores, which lose every operator override on restart.
	DatabaseURL string
	// Tenant is carried as the `app.tenant_id` GUC on every DB connection so
	// Postgres RLS scopes all reads/writes to it. Forgetting this GUC is what
	// silently emptied the accounting ledger.
	Tenant string

	// RefreshInterval is how often the projector re-resolves the golden records
	// from the vendor feeds.
	RefreshInterval time.Duration

	// AllowSim permits the dependency-free SimFeed to be the source of the golden
	// master. It is OFF by default and the service REFUSES TO START with a SimFeed
	// wired unless it is on: canned vendor data resolved into golden_records is
	// indistinguishable, once stored, from mastered reference data, and every
	// analytic downstream treats the golden record as trusted. Sim data is for a
	// developer laptop, not for anything that persists.
	AllowSim bool
}

// Load reads the configuration from the environment with production-safe
// defaults.
func Load() (Config, error) {
	return Config{
		Listen:          envOr("DATAMASTER_LISTEN", ":8080"),
		LogLevel:        parseLevel(os.Getenv("DATAMASTER_LOG_LEVEL")),
		OTLPEndpoint:    os.Getenv("DATAMASTER_OTLP_ENDPOINT"),
		DatabaseURL:     secret("DATAMASTER_DATABASE_URL"),
		Tenant:          envOr("DATAMASTER_TENANT", "__system__"),
		RefreshInterval: durationOr("DATAMASTER_REFRESH_INTERVAL", 5*time.Minute),
		AllowSim:        boolOr("DATAMASTER_ALLOW_SIM", false),
	}, nil
}

// secret resolves a sensitive value, preferring a CSI/Vault file mount (SEC-01d:
// the path in <k>_FILE) over a plaintext <k> env var — the convention the other
// service configs use. A DSN carries database credentials and must never ride in
// a pod's env block.
func secret(k string) string {
	if p := os.Getenv(k + "_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv(k)
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func boolOr(key string, def bool) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return def
	}
	return v
}

func durationOr(key string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(key)))
	if err != nil || d <= 0 {
		return def
	}
	return d
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
