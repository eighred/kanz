package config

import (
	"fmt"
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"time"

	"github.com/eighred/kanz/pkg/secret"
)

// Config is the market-data service runtime configuration. Sourced from the
// environment so it composes with the CSI/Vault secret mounts (SEC-01d).
// Mirrors the risk-engine config shape.
type Config struct {
	Listen   string
	LogLevel slog.Level

	// NATSURL is the live spine (EVT-08) the service ingests market events
	// from. Empty ⇒ HTTP-only (probes up, no ingestion).
	NATSURL string
	// Source is the consumer identity (logging / future producer use).
	Source string
	// ConsumerGroup is the durable consumer name the service subscribes under.
	ConsumerGroup string
	// Subjects are the market subjects to ingest. Default is the NATS-spine
	// wildcard `market.>` (the MARKET stream); a Kafka-backed or narrowed
	// deployment lists concrete topics/subjects (e.g. only `market.equity.bar`
	// for an EOD-only price history). Comma-separated in the environment.
	Subjects []string

	// DatabaseURL is the Postgres/Timescale DSN for the durable price history
	// (MODEL-01b). Empty ⇒ the in-memory store: ingestion works but loses
	// history on restart (local/dev).
	DatabaseURL string

	// Feed selects the market-data PUBLISHER (WIRE-01a) — the adapter that
	// streams normalized events ONTO the spine, distinct from the consumer above
	// that folds them into history. Empty ⇒ no publisher (the default; the
	// service only consumes). "sim" runs the dependency-free SimAdapter over a
	// synthetic session — the offline/local feed. A real vendor Source binds here
	// at the composition root where its SDK/creds exist (Bloomberg/Refinitiv/ICE).
	Feed string
	// FeedInstruments are the instrument_ids the publisher streams. Comma-separated.
	FeedInstruments []string
	// FeedAssetClass is the middle segment of the emitted event_type
	// (market.<assetClass>.<variant>); default "equity".
	FeedAssetClass string
	// Tenant is stamped on every market.v1 FACT the FEED publishes (MT-01b), and
	// it is the only source of one: runFeed's producer publishes from a
	// time.Ticker loop, not from an inbound delivery, so there is no ctx tenant
	// to inherit and BusSink stamps no per-event Event.TenantID.
	//
	// Without it bus.Validate refuses every tick with "tenant_id required" and
	// the publisher emits NOTHING while the service reports ready — the failure
	// market-ingest documents at its own producer and accounting's cash producer
	// shipped with (#245). Mirrors MARKET_INGEST_TENANT, which stamps the same
	// market.v1 event type.
	Tenant string

	// OTLPEndpoint is the OTel collector (host:port) for span export (OBS-01).
	OTLPEndpoint string

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as oms/venue-* do, so
	// one manifest env name serves every service.
	SPIFFESocket string

	// RollupSeries names the (instrument, venue) pairs whose 1-minute bars are
	// rolled up into the 1h and 1d series, as "BTC-USDT@XBIN,ETH-USDT@XBIN".
	//
	// EXPLICIT, NOT DISCOVERED. The bar store has no "distinct instruments" query
	// and adding one would make a routine job scan the whole table; more
	// importantly, a discovered list silently grows, and a rollup that quietly
	// starts deriving a series nobody asked for is a write nobody reviewed.
	//
	// Empty DISABLES the rollup, loudly. The coarse series then stay empty and
	// every consumer keeps reading 1-minute bars — which is the state before this
	// existed, and the startup log says so rather than leaving it to be inferred.
	RollupSeries string

	// RollupInterval is how often the rollup runs. Zero uses DefaultRollupInterval.
	RollupInterval time.Duration

	// RollupWatermarkLag is how far BEHIND now the completeness watermark sits.
	//
	// A bucket is rolled up only once it ends at or before now minus this. It
	// bounds the delay between a minute ending and its 1m bar being durable —
	// publish, consume, write, and any redelivery — and it is the one number here
	// that can produce a WRONG bar rather than a late one: too small and an hour
	// is folded before its last minutes have landed, and the result is a bar that
	// looks finished and is short.
	//
	// Zero uses DefaultRollupWatermarkLag.
	RollupWatermarkLag time.Duration
}

// DefaultSubjects is the ingestion subscription when none is configured.
var DefaultSubjects = []string{"market.>"}

func Load() (Config, error) {
	subjects := env.SplitList(os.Getenv("MARKET_DATA_SUBJECTS"))
	if len(subjects) == 0 {
		subjects = DefaultSubjects
	}

	// Resolved before the literal so a declared-but-unreadable mount stops Load
	// HERE. The local helper this replaces fell through to the plaintext env and
	// then to "", and an empty DSN is not an error in this service — it selects
	// the in-memory store. A broken CSI mount would therefore have downgraded a
	// durable price history to one that vanishes on restart, reporting a clean
	// start the whole way. See pkg/secret.
	databaseURL, err := secret.Read("MARKET_DATA_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}

	// A MALFORMED ROLLUP CADENCE REFUSES THE START (#692). The local helper this
	// replaced folded a parse error into the default, so
	// MARKET_DATA_ROLLUP_INTERVAL=1minute left the rollup on its default cadence
	// with the deployment reporting a clean start — and the candles a liquidity
	// measure reads off that series are then computed over a window nobody chose.
	//
	// A NON-POSITIVE VALUE IS STILL REFUSED, and that check moved here from the
	// helper rather than being dropped: zero is not a cadence, and it is a
	// different mistake from a typo, so it gets a different message.
	rollupInterval, err := env.Duration("MARKET_DATA_ROLLUP_INTERVAL", DefaultRollupInterval)
	if err != nil {
		return Config{}, err
	}
	if rollupInterval <= 0 {
		return Config{}, fmt.Errorf("config: MARKET_DATA_ROLLUP_INTERVAL=%s is not a positive cadence", rollupInterval)
	}
	rollupWatermarkLag, err := env.Duration("MARKET_DATA_ROLLUP_WATERMARK_LAG", DefaultRollupWatermarkLag)
	if err != nil {
		return Config{}, err
	}
	if rollupWatermarkLag <= 0 {
		return Config{}, fmt.Errorf("config: MARKET_DATA_ROLLUP_WATERMARK_LAG=%s is not a positive lag", rollupWatermarkLag)
	}

	return Config{
		Listen:        env.Or("MARKET_DATA_LISTEN", ":8082"),
		LogLevel:      env.ParseLevelOr(env.Or("MARKET_DATA_LOG_LEVEL", "info"), slog.LevelInfo),
		NATSURL:       os.Getenv("MARKET_DATA_NATS_URL"),
		Source:        env.Or("MARKET_DATA_SOURCE", "market-data"),
		ConsumerGroup: env.Or("MARKET_DATA_CONSUMER_GROUP", "market-data"),
		Subjects:      subjects,
		DatabaseURL:   databaseURL,
		OTLPEndpoint:  os.Getenv("MARKET_DATA_OTLP_ENDPOINT"),
		SPIFFESocket:  os.Getenv("SPIFFE_ENDPOINT_SOCKET"),

		Feed:            os.Getenv("MARKET_DATA_FEED"),
		FeedInstruments: env.SplitList(os.Getenv("MARKET_DATA_FEED_INSTRUMENTS")),
		FeedAssetClass:  env.Or("MARKET_DATA_FEED_ASSET_CLASS", "equity"),
		Tenant:          env.Or("MARKET_DATA_TENANT", "__system__"),

		RollupSeries:       os.Getenv("MARKET_DATA_ROLLUP_SERIES"),
		RollupInterval:     rollupInterval,
		RollupWatermarkLag: rollupWatermarkLag,
	}, nil
}

// Rollup defaults.
const (
	// DefaultRollupInterval runs the rollup hourly. The job is IDEMPOTENT and
	// looks back over a window rather than only at what is newly due, so a missed
	// run heals on the next one and the interval is a latency choice, not a
	// correctness one.
	DefaultRollupInterval = time.Hour

	// DefaultRollupWatermarkLag is ten minutes.
	//
	// It must exceed the longest plausible delay between a minute ENDING and its
	// 1-minute bar being DURABLE — market-ingest folds the live tape and
	// publishes, this service consumes and writes, and a redelivery adds a retry.
	// That path normally completes in seconds; ten minutes is deliberately
	// generous against it, because the two errors are not symmetric. Too generous
	// and the newest coarse bucket appears late. Too tight and an hour is folded
	// before its last minutes land, producing a bar that looks finished, is
	// short, and is then IMMUTABLE at that knowledge time — the store is
	// append-only, so a re-run corrects it only by writing a restatement beside
	// the wrong one.
	DefaultRollupWatermarkLag = 10 * time.Minute
)
