package config

import (
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/eighred/kanz/pkg/secret"
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

	// MarketDataURL is the Postgres/Timescale DSN of the shared market-data
	// price-history store (MODEL-01b) that the market-data service writes. When
	// set, the risk-engine reads it point-in-time-correct to drive the real
	// historical-simulation VaR99 (RISK-12), overriding the RISK-07 1%×gross
	// placeholder. Read-only from here; price history is universal market fact,
	// not tenant-owned, so the connection carries no tenant GUC. Empty ⇒ no price
	// store: VaR99 stays the placeholder (the honest no-market-data fallback).
	MarketDataURL string

	// LiquidityVenue is the MIC whose candles the liquidity measures measure ADV
	// from (RISK_ENGINE_LIQUIDITY_VENUE). REQUIRED to serve the liquidity family,
	// and there is deliberately NO DEFAULT and no "any" value.
	//
	// ADV SUMMED ACROSS VENUES IS A DIFFERENT NUMBER AND IT FLATTERS.
	// liquidity.Model.DaysToLiquidate divides size by participation × ADV, so
	// adding three venues' volume together asserts the desk can work the order on
	// all three at once and reports a liquidation horizon three times shorter than
	// any single book supports — shorter is the direction that makes a limit check
	// pass. Guessing a venue here would make that assertion on an operator's
	// behalf, in a config file nobody wrote.
	//
	// Empty ⇒ LiquidationHorizon is NOT registered and the liquidity family stays
	// dark; the composition root WARNs naming the consequence and
	// kanz_risk_measure_live{family="liquidity"} reads 0. A desk trading several
	// venues runs one deployment per venue and reconciles above this layer, where
	// the routing assumption is visible.
	LiquidityVenue string

	// OTLPEndpoint is the OTel collector (host:port) for span export (OBS-01).
	// Empty ⇒ spans are created and trace context propagates, but are not
	// exported — startup never blocks on a collector.
	OTLPEndpoint string

	// ShardMembers is the full risk-engine fleet member set (PARITY-05a) used
	// to build the consistent-hash ring. Every replica MUST be given the same
	// list. Empty or single-member ⇒ unsharded: this replica owns every
	// portfolio (the pre-05a behavior). Comma-separated in the environment.
	ShardMembers []string
	// ShardSelf is THIS replica's member id — its identity on the ring
	// (typically the StatefulSet pod name / ordinal). Must appear in
	// ShardMembers for sharding to take effect; empty ⇒ unsharded.
	ShardSelf string

	// RedisURL enables cross-pod shared-state dedup (PARITY-05c): with N
	// replicas, the consumer switches from the per-instance in-memory dedup
	// window to a Redis-backed bus.Deduper so a redelivery landing on a
	// different replica than the original is still recognized. Empty ⇒
	// per-instance dedup (the pre-05c default). Only honored in the `redis`
	// build; the default build ignores it (the concrete go-redis client is
	// behind the build tag, the PARITY-04a/04h stance).
	RedisURL string

	// Calibration (WIRE-01c): the in-process curve-calibration scheduler. The
	// risk-engine is the risk module's composition root, so it — not the
	// market-data service — wires the PARITY-03a curve.Calibrator, subscribing
	// the market quote spine into a latest-quote cache and driving Refresh on a
	// nightly + intraday cadence. Off unless CalibrationInterval is set AND
	// CalibrationRates names a rate universe.
	//
	// CalibrationInterval is the intraday refresh cadence
	// (RISK_ENGINE_CALIBRATION_INTERVAL, e.g. "5m"). Zero ⇒ scheduler disabled.
	CalibrationInterval time.Duration
	// CalibrationNightly is the nightly-close cadence
	// (RISK_ENGINE_CALIBRATION_NIGHTLY_INTERVAL); default 24h when the scheduler
	// is enabled.
	CalibrationNightly time.Duration
	// CalibrationRates is the raw rate-instrument reference spec
	// (RISK_ENGINE_CALIBRATION_RATES), parsed by livequote.ParseRateInstruments.
	CalibrationRates string
	// MarketSubjects are the market.v1 quote subjects the calibration cache
	// subscribes (RISK_ENGINE_MARKET_SUBJECTS); default the MARKET wildcard.
	// Comma-separated. Only used when the scheduler is enabled.
	MarketSubjects []string

	// GRPCListen is the address the risk query gRPC server (API-01b) binds.
	// Empty ⇒ the query server is not started (probes + ingestion only).
	GRPCListen string
	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the gRPC query server requires mTLS with an in-mesh peer SVID
	// (SEC-01b); empty ⇒ the server listens in plaintext (local/dev).
	SPIFFESocket string
}

// DefaultCalibrationNightly is the nightly-close cadence when the scheduler is
// enabled but RISK_ENGINE_CALIBRATION_NIGHTLY_INTERVAL is unset.
const DefaultCalibrationNightly = 24 * time.Hour

// DefaultMarketSubjects is the calibration cache subscription when none is set.
var DefaultMarketSubjects = []string{"market.>"}

func Load() (Config, error) {
	nightly := parseDuration(os.Getenv("RISK_ENGINE_CALIBRATION_NIGHTLY_INTERVAL"))
	if nightly <= 0 {
		nightly = DefaultCalibrationNightly
	}
	marketSubjects := splitList(os.Getenv("RISK_ENGINE_MARKET_SUBJECTS"))
	if len(marketSubjects) == 0 {
		marketSubjects = DefaultMarketSubjects
	}

	// All three are resolved before the literal so a DECLARED-but-unreadable
	// secret mount stops Load here. The local helper this replaces answered a
	// failed mount with the plaintext env var and then with "", and for every one
	// of these three "" is a DOCUMENTED, LEGITIMATE degraded mode — see the field
	// comments above: in-memory state with no restore, VaR99 back to the 1%×gross
	// placeholder, per-instance dedup instead of cross-pod. So a broken Vault
	// mount downgraded the engine into a posture that reads as deliberate, on a
	// clean start, with nothing to distinguish it from a deployment that meant it.
	//
	// That helper's own doc comment claimed the fall-through let "the downstream
	// required-field check surface the misconfiguration". There is no such check
	// here; none of the three is required. See pkg/secret.
	databaseURL, err := secret.Read("RISK_ENGINE_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	marketDataURL, err := secret.Read("RISK_ENGINE_MARKETDATA_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	redisURL, err := secret.Read("RISK_ENGINE_REDIS_URL")
	if err != nil {
		return Config{}, err
	}

	return Config{
		Listen:           envOr("RISK_ENGINE_LISTEN", ":8081"),
		LogLevel:         parseLevel(envOr("RISK_ENGINE_LOG_LEVEL", "info")),
		NATSURL:          os.Getenv("RISK_ENGINE_NATS_URL"),
		Source:           envOr("RISK_ENGINE_SOURCE", "risk-engine"),
		Tenant:           envOr("RISK_ENGINE_TENANT", "__system__"),
		DatabaseURL:      databaseURL,
		SnapshotInterval: parseDuration(os.Getenv("RISK_ENGINE_SNAPSHOT_INTERVAL")),
		KafkaBrokers:     splitList(os.Getenv("RISK_ENGINE_KAFKA_BROKERS")),
		MarketDataURL:    marketDataURL,
		LiquidityVenue:   strings.TrimSpace(os.Getenv("RISK_ENGINE_LIQUIDITY_VENUE")),
		ShardMembers:     splitList(os.Getenv("RISK_ENGINE_SHARD_MEMBERS")),
		ShardSelf:        os.Getenv("RISK_ENGINE_SHARD_SELF"),
		RedisURL:         redisURL,
		OTLPEndpoint:     os.Getenv("RISK_ENGINE_OTLP_ENDPOINT"),
		GRPCListen:       os.Getenv("RISK_ENGINE_GRPC_LISTEN"),
		SPIFFESocket:     os.Getenv("RISK_ENGINE_SPIFFE_SOCKET"),

		CalibrationInterval: parseDuration(os.Getenv("RISK_ENGINE_CALIBRATION_INTERVAL")),
		CalibrationNightly:  nightly,
		CalibrationRates:    os.Getenv("RISK_ENGINE_CALIBRATION_RATES"),
		MarketSubjects:      marketSubjects,
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
