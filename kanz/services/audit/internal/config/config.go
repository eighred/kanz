package config

import (
	"log/slog"
	"os"
	"strings"
)

// Config is the audit service runtime configuration, sourced from the
// environment so it composes with the CSI/Vault secret mounts (SEC-01d).
// Mirrors the market-data / risk-engine config shape.
type Config struct {
	Listen   string
	LogLevel slog.Level

	// NATSURL is the live spine the projection consumes. Empty ⇒ HTTP-only
	// (the query/report API serves whatever is already in the store; no new
	// projection). Useful for a read-only reporting replica.
	NATSURL string
	// Source is the consumer identity.
	Source string
	// ConsumerGroup is the durable consumer name. A single group means the
	// projection is processed once (the audit log must not double-count).
	ConsumerGroup string
	// Subjects are the subjects to materialize. Default ">" — the audit log is
	// comprehensive by design (every decision/command/outcome/quality event);
	// narrow per deployment via AUDIT_SUBJECTS only with a clear reason.
	Subjects []string

	// DatabaseURL is the Postgres DSN for the durable, WORM audit log. Empty ⇒
	// the in-memory store (local/dev; the log is lost on restart, so NOT for
	// production — an audit log that doesn't survive a restart isn't one).
	DatabaseURL string

	// OTLPEndpoint is the OTel collector for span export (OBS-01).
	OTLPEndpoint string
}

// DefaultSubjects materializes everything — audit completeness over economy.
var DefaultSubjects = []string{">"}

func Load() (Config, error) {
	subjects := splitList(os.Getenv("AUDIT_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = DefaultSubjects
	}
	return Config{
		Listen:        envOr("AUDIT_LISTEN", ":8083"),
		LogLevel:      parseLevel(envOr("AUDIT_LOG_LEVEL", "info")),
		NATSURL:       os.Getenv("AUDIT_NATS_URL"),
		Source:        envOr("AUDIT_SOURCE", "audit"),
		ConsumerGroup: envOr("AUDIT_CONSUMER_GROUP", "audit"),
		Subjects:      subjects,
		DatabaseURL:   secret("AUDIT_DATABASE_URL"),
		OTLPEndpoint:  os.Getenv("AUDIT_OTLP_ENDPOINT"),
	}, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// secret prefers a CSI/Vault file mount (<k>_FILE) over a plaintext <k> env var.
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
