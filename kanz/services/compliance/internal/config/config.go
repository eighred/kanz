package config

import (
	"log/slog"
	"os"
	"strings"

	comp "github.com/kanz-eng/kanz/internal/compliance"
	"github.com/kanz-eng/kanz/internal/platform/subject"
)

// Config is the compliance service runtime configuration, sourced from the
// environment so it composes with the CSI/Vault secret mounts (SEC-01d).
type Config struct {
	Listen       string
	LogLevel     slog.Level
	Source       string
	OTLPEndpoint string
	// NATSURL is the spine. Empty ⇒ HTTP/probes only (no consumption), a
	// read-only/degraded posture.
	NATSURL string
	// ConsumerGroup is the durable group the monitor shares.
	ConsumerGroup string
}

// PositionSubject is what the monitor BINDS: every holding's latest state
// (EXEC-M20). It is the wildcard, not the flat subject, because a position now rides
// one subject per (tenant, portfolio, instrument) on a COMPACTED stream — which is what
// lets a booting monitor rebuild the whole book in one read instead of resuming past it.
//
// subject.PositionAll lives in internal/platform/subject, not internal/risk: the OMS
// publishes it and this service consumes it, and the RISK-02 boundary forbids outsiders
// importing risk internals.
const PositionSubject = subject.PositionAll

// MonitorSubjects are the FACTs the post-trade monitor consumes: position
// changes (re-evaluate) and mandate changes (keep the registry current so a
// tightened mandate is enforced against the existing book).
func (Config) MonitorSubjects() []string {
	return []string{PositionSubject}
}

// MandateSubject is the shared mandate ConfigChanged stream.
func (Config) MandateSubject() string { return comp.SubjectMandateChanged }

func Load() (Config, error) {
	return Config{
		Listen:        envOr("COMPLIANCE_LISTEN", ":8091"),
		LogLevel:      parseLevel(envOr("COMPLIANCE_LOG_LEVEL", "info")),
		Source:        envOr("COMPLIANCE_SOURCE", "compliance"),
		OTLPEndpoint:  os.Getenv("COMPLIANCE_OTLP_ENDPOINT"),
		NATSURL:       os.Getenv("COMPLIANCE_NATS_URL"),
		ConsumerGroup: envOr("COMPLIANCE_CONSUMER_GROUP", "compliance"),
	}, nil
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
