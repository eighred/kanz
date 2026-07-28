package config

import (
	"log/slog"
	"os"
	"strings"

	"github.com/kanz-eng/kanz/pkg/secret"
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

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as oms/venue-* do, so
	// one manifest env name serves every service.
	SPIFFESocket string
}

// DefaultSubjects materializes everything — audit completeness over economy.
var DefaultSubjects = []string{">"}

func Load() (Config, error) {
	subjects := splitList(os.Getenv("AUDIT_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = DefaultSubjects
	}
	// Resolved before the literal so a declared-but-unreadable mount stops Load
	// here. An empty DSN is a LEGAL value for this service — it selects the
	// in-memory store — so a broken Vault mount used to produce an audit
	// service that started clean, served queries, and lost the entire tamper-
	// evidence log on restart. Nothing downstream would have reported it.
	databaseURL, err := secret.Read("AUDIT_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	return Config{
		Listen:        envOr("AUDIT_LISTEN", ":8083"),
		LogLevel:      parseLevel(envOr("AUDIT_LOG_LEVEL", "info")),
		NATSURL:       os.Getenv("AUDIT_NATS_URL"),
		Source:        envOr("AUDIT_SOURCE", "audit"),
		ConsumerGroup: envOr("AUDIT_CONSUMER_GROUP", "audit"),
		Subjects:      subjects,
		DatabaseURL:   databaseURL,
		OTLPEndpoint:  os.Getenv("AUDIT_OTLP_ENDPOINT"),
		SPIFFESocket:  os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
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
