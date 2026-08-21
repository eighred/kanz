package config

import (
	"errors"
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"strings"
)

// Config is the lake-sink runtime configuration, sourced from the environment so
// it composes with the CSI/Vault secret mounts (SEC-01d). Mirrors the audit /
// risk-engine config shape.
type Config struct {
	Listen   string
	LogLevel slog.Level

	// Brokers is the Kafka cluster — the durable log of record (EVT-09) the sink
	// reads as its CDC source. Required: the sink streams the durable log, not the
	// live NATS spine, so a replay rebuilds the lakehouse deterministically.
	Brokers []string
	// Topics are the `{domain}.{entity}` topics to land. Kafka has no subject
	// wildcard, so the set is explicit per deployment. Required.
	Topics []string
	// Source is the consumer identity.
	Source string
	// ConsumerGroup is the durable consumer name — one group so the log is landed
	// once (a second group would double-write the lake).
	ConsumerGroup string

	// RegistryURL is the schema-registry (EVT-16) base URL the decoder resolves
	// payload descriptors against. Empty ⇒ envelope-only landing (payloads kept
	// raw-undecoded); set it to get schema-evolution-aware decoded columns.
	RegistryURL string

	// OutputDir is the FileSink landing root (Hive-partitioned NDJSON the
	// catalog ingest commits as a table). Required by the default sink.
	OutputDir string

	// OTLPEndpoint is the OTel collector for span export (OBS-01).
	OTLPEndpoint string
}

func Load() (Config, error) {
	cfg := Config{
		Listen:        env.Or("LAKE_SINK_LISTEN", ":8085"),
		LogLevel:      env.ParseLevelOr(env.Or("LAKE_SINK_LOG_LEVEL", "info"), slog.LevelInfo),
		Brokers:       env.SplitList(os.Getenv("LAKE_SINK_BROKERS")),
		Topics:        env.SplitList(os.Getenv("LAKE_SINK_TOPICS")),
		Source:        env.Or("LAKE_SINK_SOURCE", "lake-sink"),
		ConsumerGroup: env.Or("LAKE_SINK_CONSUMER_GROUP", "lake-sink"),
		RegistryURL:   strings.TrimRight(os.Getenv("LAKE_SINK_REGISTRY_URL"), "/"),
		OutputDir:     os.Getenv("LAKE_SINK_OUTPUT_DIR"),
		OTLPEndpoint:  os.Getenv("LAKE_SINK_OTLP_ENDPOINT"),
	}
	if len(cfg.Brokers) == 0 {
		return Config{}, errors.New("LAKE_SINK_BROKERS is required")
	}
	if len(cfg.Topics) == 0 {
		return Config{}, errors.New("LAKE_SINK_TOPICS is required")
	}
	if cfg.OutputDir == "" {
		return Config{}, errors.New("LAKE_SINK_OUTPUT_DIR is required")
	}
	return cfg, nil
}
