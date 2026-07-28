package config

import (
	"log/slog"
	"os"
	"strings"

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

	// OTLPEndpoint is the OTel collector (host:port) for span export (OBS-01).
	OTLPEndpoint string

	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When
	// set, the bus dials the spine over mTLS presenting this workload SVID; empty
	// means a PLAINTEXT dial, which the production broker refuses at the
	// handshake (SEC-M3). Reads the go-spiffe standard env, as oms/venue-* do, so
	// one manifest env name serves every service.
	SPIFFESocket string
}

// DefaultSubjects is the ingestion subscription when none is configured.
var DefaultSubjects = []string{"market.>"}

func Load() (Config, error) {
	subjects := splitList(os.Getenv("MARKET_DATA_SUBJECTS"))
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

	return Config{
		Listen:        envOr("MARKET_DATA_LISTEN", ":8082"),
		LogLevel:      parseLevel(envOr("MARKET_DATA_LOG_LEVEL", "info")),
		NATSURL:       os.Getenv("MARKET_DATA_NATS_URL"),
		Source:        envOr("MARKET_DATA_SOURCE", "market-data"),
		ConsumerGroup: envOr("MARKET_DATA_CONSUMER_GROUP", "market-data"),
		Subjects:      subjects,
		DatabaseURL:   databaseURL,
		OTLPEndpoint:  os.Getenv("MARKET_DATA_OTLP_ENDPOINT"),
		SPIFFESocket:  os.Getenv("SPIFFE_ENDPOINT_SOCKET"),

		Feed:            os.Getenv("MARKET_DATA_FEED"),
		FeedInstruments: splitList(os.Getenv("MARKET_DATA_FEED_INSTRUMENTS")),
		FeedAssetClass:  envOr("MARKET_DATA_FEED_ASSET_CLASS", "equity"),
	}, nil
}

// splitList parses a comma-separated env value into a trimmed, non-empty slice;
// an empty or all-whitespace value yields nil.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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
