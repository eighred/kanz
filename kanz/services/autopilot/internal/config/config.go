package config

import (
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"strings"
)

// Config is the autopilot runtime configuration, sourced from the environment.
// Same shape as the audit/lineage services (autopilot is their event-driven
// sibling — a bus consumer + control loop).
type Config struct {
	Listen   string
	LogLevel slog.Level

	// NATSURL is the live spine the controller consumes signals from. Empty ⇒
	// HTTP-only (probes/metrics; no control loop).
	NATSURL string
	// Source is the consumer identity.
	Source string
	// ConsumerGroup is the durable consumer name (one group ⇒ each signal acted
	// on once).
	ConsumerGroup string
	// Subjects to watch for signals. Default the observation + platform domains
	// where DATA-07 quality events, SLO-burn, and circuit-breaker events ride;
	// narrow per deployment.
	Subjects []string

	// AutoFailover gates autonomous DR-01d regional failover on a sustained
	// critical SLO burn (the hardest-to-reverse action). Off by default — failover
	// escalates to a human unless explicitly enabled (AUTO-01d).
	AutoFailover bool

	// OTLPEndpoint is the OTel collector for span export (OBS-01).
	OTLPEndpoint string

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as oms/venue-* do, so
	// one manifest env name serves every service.
	SPIFFESocket string
}

// DefaultSubjects watches the domains operational signals are published on.
var DefaultSubjects = []string{"observation.>", "platform.>", "data.>"}

func Load() (Config, error) {
	subjects := env.SplitList(os.Getenv("AUTOPILOT_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = DefaultSubjects
	}
	return Config{
		Listen:        env.Or("AUTOPILOT_LISTEN", ":8087"),
		LogLevel:      env.ParseLevelOr(env.Or("AUTOPILOT_LOG_LEVEL", "info"), slog.LevelInfo),
		NATSURL:       os.Getenv("AUTOPILOT_NATS_URL"),
		Source:        env.Or("AUTOPILOT_SOURCE", "autopilot"),
		ConsumerGroup: env.Or("AUTOPILOT_CONSUMER_GROUP", "autopilot"),
		Subjects:      subjects,
		AutoFailover:  truthy(os.Getenv("AUTOPILOT_AUTO_FAILOVER")),
		OTLPEndpoint:  os.Getenv("AUTOPILOT_OTLP_ENDPOINT"),
		SPIFFESocket:  os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
	}, nil
}
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
