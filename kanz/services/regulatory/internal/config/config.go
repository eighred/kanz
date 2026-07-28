package config

import (
	"log/slog"
	"os"
	"strings"

	"github.com/kanz-eng/kanz/pkg/secret"
)

// Config is the regulatory filing service runtime configuration, sourced from
// the environment. The service assembles the delivered FRTB / Form PF / AIFMD /
// TCFD / SFDR filings from request-supplied inputs, signs them, and serves them
// point-in-time + completeness-gated — the reporting layer over the PARITY-06
// analytics (WIRE-01e).
type Config struct {
	Listen   string
	LogLevel slog.Level

	// Signer selects the filing signature backend: "chain" (default) links every
	// filing into the AUDIT-01 hash chain (signer.ChainSigner) so a signature is
	// a verifiable chain position; "hash" is a bare SHA-256 content digest
	// (tamper-evidence without ordering). Both satisfy the delivered Signer seam.
	Signer string

	// DatabaseURL is the Postgres DSN for the durable audit hash-chain link store
	// (REG-02). When set (and Signer is "chain"), each filing's chain link is
	// persisted and the chain head is recovered on startup, so the tamper-evident
	// chain survives a restart. Empty ⇒ links chain in memory only (verifiable
	// within the process, lost on restart) — the honest single-replica default.
	DatabaseURL string

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string
}

// Load reads the configuration from the environment with production-safe defaults.
func Load() (Config, error) {
	// Resolved before the literal so a declared-but-unreadable mount stops Load
	// HERE. The local helper this replaces answered an unreadable file with the
	// plaintext env and then with "", and "" is this service's in-memory-chain
	// default: a broken CSI mount would have quietly demoted the durable audit
	// hash chain (REG-02) to one lost on the next restart, which is precisely the
	// tamper-evidence the filings are signed to carry. See pkg/secret.
	databaseURL, err := secret.Read("REGULATORY_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}

	return Config{
		Listen:       envOr("REGULATORY_LISTEN", ":8083"),
		LogLevel:     parseLevel(os.Getenv("REGULATORY_LOG_LEVEL")),
		Signer:       strings.ToLower(envOr("REGULATORY_SIGNER", "chain")),
		DatabaseURL:  databaseURL,
		OTLPEndpoint: os.Getenv("REGULATORY_OTLP_ENDPOINT"),
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
