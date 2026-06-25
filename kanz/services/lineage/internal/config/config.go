package config

import (
	"log/slog"
	"os"
	"strings"
)

// Config is the lineage service runtime configuration, sourced from the
// environment so it composes with the CSI/Vault secret + ConfigMap mounts. Same
// shape as the audit service (LIN-01 is its lineage-graph sibling).
type Config struct {
	Listen   string
	LogLevel slog.Level

	// NATSURL is the live spine the harvester consumes. Empty ⇒ HTTP-only (serve
	// the lineage/catalog API over whatever graph already exists — e.g. a
	// read-only replica).
	NATSURL string
	// Source is the consumer identity.
	Source string
	// ConsumerGroup is the durable consumer name (one group ⇒ harvested once).
	ConsumerGroup string
	// Subjects to harvest. Default ">" — the lineage graph is comprehensive by
	// design, like the audit log.
	Subjects []string

	// PolicyFile is the AUTH-01b policy bundle governing PII lineage access.
	// Empty ⇒ a deny-all authorizer (no role can read PII until a bundle is
	// mounted) — deny-by-default at the config layer too.
	PolicyFile string
	// GovernanceFile is the LIN-01c PII classification config. Empty ⇒ nothing is
	// classified PII (everything public) — a deployment must mount it to govern.
	GovernanceFile string

	// OpenLineageURL is the OpenLineage backend (Marquez/DataHub) the emitter
	// POSTs to. Empty ⇒ the LogEmitter (events to stdout JSON).
	OpenLineageURL string

	// OTLPEndpoint is the OTel collector for span export (OBS-01).
	OTLPEndpoint string
}

// DefaultSubjects harvests everything — lineage completeness over economy.
var DefaultSubjects = []string{">"}

func Load() (Config, error) {
	subjects := splitList(os.Getenv("LINEAGE_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = DefaultSubjects
	}
	return Config{
		Listen:         envOr("LINEAGE_LISTEN", ":8086"),
		LogLevel:       parseLevel(envOr("LINEAGE_LOG_LEVEL", "info")),
		NATSURL:        os.Getenv("LINEAGE_NATS_URL"),
		Source:         envOr("LINEAGE_SOURCE", "lineage"),
		ConsumerGroup:  envOr("LINEAGE_CONSUMER_GROUP", "lineage"),
		Subjects:       subjects,
		PolicyFile:     os.Getenv("LINEAGE_POLICY_FILE"),
		GovernanceFile: os.Getenv("LINEAGE_GOVERNANCE_FILE"),
		OpenLineageURL: strings.TrimRight(os.Getenv("LINEAGE_OPENLINEAGE_URL"), "/"),
		OTLPEndpoint:   os.Getenv("LINEAGE_OTLP_ENDPOINT"),
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
