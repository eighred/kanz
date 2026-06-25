package config

import (
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
}

// DefaultSubjects watches the domains operational signals are published on.
var DefaultSubjects = []string{"observation.>", "platform.>", "data.>"}

func Load() (Config, error) {
	subjects := splitList(os.Getenv("AUTOPILOT_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = DefaultSubjects
	}
	return Config{
		Listen:        envOr("AUTOPILOT_LISTEN", ":8087"),
		LogLevel:      parseLevel(envOr("AUTOPILOT_LOG_LEVEL", "info")),
		NATSURL:       os.Getenv("AUTOPILOT_NATS_URL"),
		Source:        envOr("AUTOPILOT_SOURCE", "autopilot"),
		ConsumerGroup: envOr("AUTOPILOT_CONSUMER_GROUP", "autopilot"),
		Subjects:      subjects,
		AutoFailover:  truthy(os.Getenv("AUTOPILOT_AUTO_FAILOVER")),
		OTLPEndpoint:  os.Getenv("AUTOPILOT_OTLP_ENDPOINT"),
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

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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
