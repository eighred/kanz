package config

import (
	"log/slog"
	"os"
	"strings"
	"time"
)

// Config is the risk-engine runtime configuration. Sourced from the
// environment so it composes with the CSI/Vault secret mounts (SEC-01d).
type Config struct {
	Listen   string
	LogLevel slog.Level

	// NATSURL is the live spine (EVT-08). Empty ⇒ the service runs
	// HTTP-only (probes up, no ingestion) — useful for a bare scaffold
	// deploy or local boot without a broker.
	NATSURL string
	// Source is the producer identity stamped on emitted FACTs (EVT-17b).
	Source string
	// Tenant is the tenant_id stamped on emitted FACTs (MT-01a). The recomputer
	// publishes derived risk events from a background ctx (debounced, async), so
	// the triggering event's tenant can't propagate via ctx here — this is the
	// fallback. Defaults to the reserved "__system__"; a single-tenant
	// deployment sets RISK_ENGINE_TENANT. True per-portfolio tenant stamping
	// (the engine carrying portfolio→tenant in state) is an MT-01d follow-up.
	Tenant string

	// DatabaseURL is the Postgres DSN for durable state (PERS-01). Empty ⇒
	// state is in-memory only: no bootstrap restore, no periodic snapshot,
	// re-baselines from the live spine on restart.
	DatabaseURL string
	// SnapshotInterval tunes the periodic durable-snapshot cadence (PARITY-02f):
	// shorter shrinks restart-to-ready replay at the cost of more SQL write
	// volume, longer suits quiet books. A deploy-time knob so cadence tuning
	// needs no rebuild. Zero (unset/unparseable) ⇒ engine.DefaultSnapshotInterval.
	SnapshotInterval time.Duration
	// KafkaBrokers is the durable-log bootstrap list (EVT-09) used by the
	// PERS-01d bootstrap replay. Empty ⇒ replay is skipped; the engine
	// restores from the latest durable snapshot and relies on the live NATS
	// spine to cover everything since. Comma-separated in the environment.
	KafkaBrokers []string

	// OTLPEndpoint is the OTel collector (host:port) for span export (OBS-01).
	// Empty ⇒ spans are created and trace context propagates, but are not
	// exported — startup never blocks on a collector.
	OTLPEndpoint string

	// GRPCListen is the address the risk query gRPC server (API-01b) binds.
	// Empty ⇒ the query server is not started (probes + ingestion only).
	GRPCListen string
	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the gRPC query server requires mTLS with an in-mesh peer SVID
	// (SEC-01b); empty ⇒ the server listens in plaintext (local/dev).
	SPIFFESocket string
}

func Load() (Config, error) {
	return Config{
		Listen:           envOr("RISK_ENGINE_LISTEN", ":8081"),
		LogLevel:         parseLevel(envOr("RISK_ENGINE_LOG_LEVEL", "info")),
		NATSURL:          os.Getenv("RISK_ENGINE_NATS_URL"),
		Source:           envOr("RISK_ENGINE_SOURCE", "risk-engine"),
		Tenant:           envOr("RISK_ENGINE_TENANT", "__system__"),
		DatabaseURL:      secret("RISK_ENGINE_DATABASE_URL"),
		SnapshotInterval: parseDuration(os.Getenv("RISK_ENGINE_SNAPSHOT_INTERVAL")),
		KafkaBrokers:     splitList(os.Getenv("RISK_ENGINE_KAFKA_BROKERS")),
		OTLPEndpoint:     os.Getenv("RISK_ENGINE_OTLP_ENDPOINT"),
		GRPCListen:       os.Getenv("RISK_ENGINE_GRPC_LISTEN"),
		SPIFFESocket:     os.Getenv("RISK_ENGINE_SPIFFE_SOCKET"),
	}, nil
}

// splitList parses a comma-separated env value into a trimmed, non-empty
// slice; an empty or all-whitespace value yields nil.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// secret resolves a sensitive value, preferring a CSI/Vault file mount
// (SEC-01d: the path in <k>_FILE) over a plaintext <k> env var. The DSN is
// thus never a plaintext value in the pod spec or etcd. Empty when neither is
// set; a file path that fails to read falls through to the env var so the
// downstream required-field check surfaces the misconfiguration.
func secret(k string) string {
	if p := os.Getenv(k + "_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv(k)
}

// parseDuration parses a Go duration (e.g. "30s", "2m"); an empty or malformed
// value yields 0, which the snapshotter maps to engine.DefaultSnapshotInterval.
func parseDuration(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
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
