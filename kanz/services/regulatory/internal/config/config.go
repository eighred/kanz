package config

import (
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"strings"

	"github.com/eighred/kanz/pkg/secret"
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
	// chain survives a restart. Empty ⇒ openLinkStore REFUSES TO START unless
	// AllowEphemeralChain says the deployment accepts an in-process chain (#261).
	// Signer "hash" needs no DSN at all and never reaches that check.
	DatabaseURL string
	// AllowEphemeralChain (REGULATORY_ALLOW_EPHEMERAL_CHAIN=true) opts in to the
	// in-memory link store when the chain signer is selected and no DSN is set.
	// It exists for a laptop and a test rig; no shipped manifest sets it, and a
	// deployment that does MUST run one replica — see openLinkStore.
	AllowEphemeralChain bool

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
		Listen:       env.Or("REGULATORY_LISTEN", ":8083"),
		LogLevel:     env.ParseLevelOr(os.Getenv("REGULATORY_LOG_LEVEL"), slog.LevelInfo),
		Signer:       strings.ToLower(env.Or("REGULATORY_SIGNER", "chain")),
		DatabaseURL:  databaseURL,
		OTLPEndpoint: os.Getenv("REGULATORY_OTLP_ENDPOINT"),

		AllowEphemeralChain: os.Getenv("REGULATORY_ALLOW_EPHEMERAL_CHAIN") == "true",
	}, nil
}
