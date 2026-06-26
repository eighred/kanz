package config

import (
	"log/slog"
	"os"
	"strings"

	"github.com/kanz-eng/kanz/services/oms/internal/order"
)

// Config is the oms runtime configuration, sourced from the environment so it
// composes with the CSI/Vault secret mounts (SEC-01d).
type Config struct {
	Listen   string
	LogLevel slog.Level
	Source   string
	// OTLPEndpoint is the OTel collector for span export (OBS-01).
	OTLPEndpoint string
	// NATSURL is the spine. Empty ⇒ the service runs HTTP/probes only (no
	// command consumption), a read-only/degraded posture.
	NATSURL string
	// ConsumerGroup is the durable group the command + fill consumers share.
	ConsumerGroup string
	// SimVenueMIC is the simulation execution venue's MIC (OMS-01c). Empty ⇒ a
	// default sim venue; a deployment swaps in a real venue adapter.
	SimVenueMIC string
	// BaseCurrency stamps Money on projected positions until a reference-data
	// currency join lands (OMS-01e).
	BaseCurrency string
}

// CommandSubjects are the order command subjects the OMS consumes.
func (Config) CommandSubjects() []string {
	return []string{order.SubjectSubmit, order.SubjectAmend, order.SubjectCancel}
}

// FillSubjects are the fill FACTs the position projector consumes (OMS-01e).
func (Config) FillSubjects() []string {
	return []string{order.EventTypePartiallyFilled, order.EventTypeFilled}
}

func Load() (Config, error) {
	return Config{
		Listen:        envOr("OMS_LISTEN", ":8090"),
		LogLevel:      parseLevel(envOr("OMS_LOG_LEVEL", "info")),
		Source:        envOr("OMS_SOURCE", "oms"),
		OTLPEndpoint:  os.Getenv("OMS_OTLP_ENDPOINT"),
		NATSURL:       os.Getenv("OMS_NATS_URL"),
		ConsumerGroup: envOr("OMS_CONSUMER_GROUP", "oms"),
		SimVenueMIC:   envOr("OMS_SIM_VENUE_MIC", "XSIM"),
		BaseCurrency:  envOr("OMS_BASE_CURRENCY", "USD"),
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
