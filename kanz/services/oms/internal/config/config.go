package config

import (
	"errors"
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
	// DatabaseURL is the durable order store (EXEC-M7c). Empty ⇒ the in-memory
	// store, whose admission gate is a process-local mutex — correct for tests
	// and a single replica ONLY. A multi-replica deployment MUST set this: two
	// pods over two in-memory maps both admit the same order_id and both route
	// it to the venue.
	DatabaseURL string
	// Tenant is the owning tenant of this OMS deployment, carried as the
	// `app.tenant_id` GUC on every DB connection so Postgres RLS scopes the order
	// store (MT-01d). Required whenever DatabaseURL is set — RLS is fail-closed.
	Tenant string
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
	cfg := Config{
		Listen:        envOr("OMS_LISTEN", ":8090"),
		LogLevel:      parseLevel(envOr("OMS_LOG_LEVEL", "info")),
		Source:        envOr("OMS_SOURCE", "oms"),
		OTLPEndpoint:  os.Getenv("OMS_OTLP_ENDPOINT"),
		NATSURL:       os.Getenv("OMS_NATS_URL"),
		ConsumerGroup: envOr("OMS_CONSUMER_GROUP", "oms"),
		DatabaseURL:   os.Getenv("OMS_DATABASE_URL"),
		Tenant:        os.Getenv("OMS_TENANT"),
		SimVenueMIC:   envOr("OMS_SIM_VENUE_MIC", "XSIM"),
		BaseCurrency:  envOr("OMS_BASE_CURRENCY", "USD"),
	}
	// Fail fast rather than fail silently: RLS is deny-by-default, so an unset
	// tenant against a real database yields a store that reads back nothing.
	if cfg.DatabaseURL != "" && cfg.Tenant == "" {
		return Config{}, errors.New("oms: OMS_TENANT is required when OMS_DATABASE_URL is set (RLS scoping)")
	}
	return cfg, nil
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
