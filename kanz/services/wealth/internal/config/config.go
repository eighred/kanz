package config

import (
	"log/slog"
	"os"
	"strings"

	"github.com/eighred/kanz/internal/wealth"
	"github.com/eighred/kanz/pkg/secret"
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

	// NATSURL is the live spine the household-valuation consumer subscribes to
	// (WEALTH-01b). Empty ⇒ no consumer (the default; the service serves the
	// read endpoints on whatever store openStore selected, without a broker).
	NATSURL string
	// Source is the consumer identity (logging / durable consumer name).
	Source string
	// ConsumerGroup is the durable consumer name the household subject
	// subscribes under.
	ConsumerGroup string
	// Subjects are the wealth.v1.HouseholdValued FACT subjects folded into the
	// book. Defaults to wealth.SubjectHouseholdAll — the compacted wildcard over
	// every household. Comma-separated.
	Subjects []string

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as accounting/
	// alternatives do, so one manifest env name serves every service.
	SPIFFESocket string
}

// Load reads the configuration from the environment with production-safe
// defaults.
func Load() (Config, error) {
	subjects := splitList(os.Getenv("WEALTH_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = []string{wealth.SubjectHouseholdAll}
	}

	// The DSN carries database credentials and rides a CSI/Vault file mount
	// (SEC-01d), never a pod's env block. Resolved before the literal so an
	// unreadable mount stops Load HERE: nothing downstream would have caught it,
	// because "" is a legal value that selects the in-memory book — a failed mount
	// would have started a wealth service that serves households correctly until
	// the first restart drops every one of them. See pkg/secret.
	databaseURL, err := secret.Read("WEALTH_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}

	return Config{
		Listen:        envOr("WEALTH_LISTEN", ":8080"),
		LogLevel:      parseLevel(os.Getenv("WEALTH_LOG_LEVEL")),
		OTLPEndpoint:  os.Getenv("WEALTH_OTLP_ENDPOINT"),
		DatabaseURL:   databaseURL,
		Tenant:        envOr("WEALTH_TENANT", "__system__"),
		NATSURL:       os.Getenv("WEALTH_NATS_URL"),
		Source:        envOr("WEALTH_SOURCE", "wealth"),
		ConsumerGroup: envOr("WEALTH_CONSUMER_GROUP", "wealth"),
		Subjects:      subjects,
		SPIFFESocket:  os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
	}, nil
}

// splitList parses a comma-separated env value into a trimmed, non-empty slice.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
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
