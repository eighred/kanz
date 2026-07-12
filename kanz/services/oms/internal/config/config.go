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
	// DatabaseURL is the durable order store (EXEC-M7c). Empty ⇒ the in-memory
	// store, whose admission gate is a process-local mutex — correct for tests
	// and a single replica ONLY. A multi-replica deployment MUST set this: two
	// pods over two in-memory maps both admit the same order_id and both route
	// it to the venue.
	DatabaseURL string
	// Tenant is the owning tenant of this OMS deployment, carried as the
	// `app.tenant_id` GUC on every DB connection so Postgres RLS scopes the order
	// store (MT-01d). Defaults to __system__, the risk-engine convention; a
	// per-tenant deployment overrides it.
	Tenant string
	// SimVenueMIC is the simulation execution venue's MIC, or a COMMA-SEPARATED
	// LIST of them ("XNAS,XLON") — one SimVenue per MIC. A fanned-out allocation
	// stamps each leg with its target venue and the router matches on MIC, so a
	// single-venue simulator cannot work a multi-venue allocation at all. Empty ⇒ a
	// default sim venue; a deployment swaps in a real venue adapter.
	SimVenueMIC string
	// VenueEndpoints maps MIC → adapter address for OUT-OF-PROCESS venues
	// (INFRA-M7a): "XBIN=venue-binance.kanz-services.svc:9000,XOKX=venue-okx...".
	// Each becomes an execution.GRPCVenue. This is how a venue reaches the OMS
	// without a line of vendor code being linked into it.
	VenueEndpoints string
	// SPIFFESocket is the workload API socket used to mTLS the venue dials
	// (SEC-01a). Empty ⇒ plaintext, which is a DEV-ONLY posture: the venue
	// connection carries live orders.
	SPIFFESocket string
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
		Listen:         envOr("OMS_LISTEN", ":8090"),
		LogLevel:       parseLevel(envOr("OMS_LOG_LEVEL", "info")),
		Source:         envOr("OMS_SOURCE", "oms"),
		OTLPEndpoint:   os.Getenv("OMS_OTLP_ENDPOINT"),
		NATSURL:        os.Getenv("OMS_NATS_URL"),
		ConsumerGroup:  envOr("OMS_CONSUMER_GROUP", "oms"),
		DatabaseURL:    secret("OMS_DATABASE_URL"),
		Tenant:         envOr("OMS_TENANT", "__system__"),
		SimVenueMIC:    envOr("OMS_SIM_VENUE_MIC", "XSIM"),
		VenueEndpoints: os.Getenv("OMS_VENUE_ENDPOINTS"),
		SPIFFESocket:   os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		BaseCurrency:   envOr("OMS_BASE_CURRENCY", "USD"),
	}
	return cfg, nil
}

// secret resolves a sensitive value, preferring a CSI/Vault file mount
// (SEC-01d: the path in <k>_FILE) over a plaintext <k> env var. The DSN carries
// database credentials and must never ride in a pod's env block.
func secret(k string) string {
	if p := os.Getenv(k + "_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv(k)
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
