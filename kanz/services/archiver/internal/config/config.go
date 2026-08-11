// Package config is the archiver's runtime configuration, sourced from the
// environment so it composes with the CSI/Vault secret mounts (SEC-01d). Mirrors
// the lake-sink / risk-engine config shape.
package config

import (
	"errors"
	"log/slog"
	"os"
	"strings"
)

// DefaultSubjects is what DATA-M1 archives: every stream carrying history that
// cannot be reconstructed from anywhere else.
//
// market.> is OUT on purpose — ticks are the highest-volume streams by far and are
// re-fetchable from the venue, unlike a fill. observability.> is OUT because it is
// a 1h ephemeral stream and nothing declares a subject on it.
var DefaultSubjects = []string{
	"order.>",
	"strategy.>",
	// optimization.> (#409): the FACT naming who authorized a rebalance that
	// became live orders. It is archived for the same reason order.> above it is
	// — it is not reconstructable from anywhere else, and it is the ONLY record
	// of who released an automated capital action. Losing it in a failover would
	// leave the restored order history with no statement of who authorized it.
	"optimization.>",
	"execution.>",
	"accounting.>",
	"settlement.>",
	"compliance.breach.>",
	"compliance.mandate.>",
	"risk.portfolio.>",
	"risk.exposure.>",
	"risk.signal.>",
	"risk.command.>",
	"risk.position.>",
	"inference.>",
	"platform.>",
	"data.>",
}

// Config is the archiver's runtime configuration.
type Config struct {
	Listen   string
	LogLevel slog.Level

	// Tenant is the tenant this archiver serves. REQUIRED: NATS isolation is by
	// account, and the Kafka topic is tenant-prefixed. An archiver that does not
	// know its tenant cannot route a single event.
	Tenant string
	// NATSURL is the live spine it drains. REQUIRED.
	NATSURL string
	// Brokers is the Kafka cluster it archives INTO. REQUIRED: an archiver with no
	// Kafka reports healthy and retains nothing.
	Brokers []string
	// Subjects are the NATS subject patterns to archive.
	Subjects []string
	// Group is the durable consumer name.
	Group string
	// Source is the service identity stamped on telemetry.
	Source string
	// OTLPEndpoint is the OTel collector for span export (OBS-01).
	OTLPEndpoint string

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as oms/venue-* do, so
	// one manifest env name serves every service.
	SPIFFESocket string
}

// Load reads the environment and REFUSES to return a config that cannot archive.
func Load() (Config, error) {
	cfg := Config{
		Listen:       envOr("ARCHIVER_LISTEN", ":8086"),
		LogLevel:     parseLevel(envOr("ARCHIVER_LOG_LEVEL", "info")),
		Tenant:       os.Getenv("ARCHIVER_TENANT"),
		NATSURL:      os.Getenv("ARCHIVER_NATS_URL"),
		Brokers:      splitList(os.Getenv("ARCHIVER_KAFKA_BROKERS")),
		Subjects:     splitList(os.Getenv("ARCHIVER_SUBJECTS")),
		Group:        envOr("ARCHIVER_CONSUMER_GROUP", "archiver"),
		Source:       envOr("ARCHIVER_SOURCE", "archiver"),
		OTLPEndpoint: os.Getenv("ARCHIVER_OTLP_ENDPOINT"),
		SPIFFESocket: os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
	}
	if len(cfg.Subjects) == 0 {
		cfg.Subjects = DefaultSubjects
	}
	if cfg.Tenant == "" {
		return Config{}, errors.New("ARCHIVER_TENANT is required: an archiver that does not know its tenant cannot route an event")
	}
	if cfg.NATSURL == "" {
		return Config{}, errors.New("ARCHIVER_NATS_URL is required")
	}
	if len(cfg.Brokers) == 0 {
		return Config{}, errors.New("ARCHIVER_KAFKA_BROKERS is required: an archiver with no Kafka reports healthy and retains nothing")
	}
	return cfg, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
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
