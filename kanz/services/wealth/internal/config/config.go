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

	// DatabaseURL selects the durable household book. Empty ⇒ the in-memory store,
	// which is correct for tests and a single replica but loses every household on
	// restart. book.Postgres has existed since PARITY-02b; nothing constructed it
	// until now.
	DatabaseURL string
	// Tenant is carried as the `app.tenant_id` GUC on every DB connection so
	// Postgres RLS scopes the book (MT-01d). Defaults to __system__, the
	// risk-engine convention.
	Tenant string
}

// Load reads the configuration from the environment with production-safe
// defaults.
func Load() (Config, error) {
	return Config{
		Listen:       envOr("WEALTH_LISTEN", ":8080"),
		LogLevel:     parseLevel(os.Getenv("WEALTH_LOG_LEVEL")),
		OTLPEndpoint: os.Getenv("WEALTH_OTLP_ENDPOINT"),
		DatabaseURL:  secret("WEALTH_DATABASE_URL"),
		Tenant:       envOr("WEALTH_TENANT", "__system__"),
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
