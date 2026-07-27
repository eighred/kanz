// nats-rebuild reconstructs the NATS live spine from the Kafka log of record
// (DR-01c). Run it once in the DR region after the NATS streams are provisioned
// (the bootstrap-job) and the DR-replicated Kafka log is present (DR-01a): it
// drains a bounded recent window of each log-of-record topic back onto the live
// subjects, then exits. Idempotent re-runs are safe (the stream dedup window +
// idempotent handlers), so a partial rebuild can simply be re-run.
//
// Kafka is read plaintext over the in-cluster listener (kafka:9092), like the
// provisioning Jobs; NATS publish goes over SEC-01c mTLS when a SPIFFE socket is
// set (required against a verify:true cluster), plaintext otherwise (dev).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/transport"
	"github.com/kanz-eng/kanz/tools/natsrebuild"
	"github.com/kanz-eng/kanz/tools/replay"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	brokers := splitList(envOr("NATS_REBUILD_KAFKA_BROKERS", "kafka:9092"))
	natsURL := envOr("NATS_REBUILD_NATS_URL", "nats://nats:4222")
	topics := splitList(os.Getenv("NATS_REBUILD_TOPICS"))
	stateTopics := splitList(os.Getenv("NATS_REBUILD_STATE_TOPICS"))
	socket := os.Getenv("NATS_REBUILD_SPIFFE_SOCKET")
	since, err := time.ParseDuration(envOr("NATS_REBUILD_SINCE", "24h"))
	if err != nil {
		logger.Error("invalid NATS_REBUILD_SINCE", "err", err)
		os.Exit(2)
	}
	if len(topics) == 0 {
		logger.Error("NATS_REBUILD_TOPICS is required (the log-of-record topics to rebuild from)")
		os.Exit(2)
	}
	if err := natsrebuild.RequireStateTopics(stateTopics); err != nil {
		logger.Error("invalid state-topic configuration", "err", err)
		os.Exit(2)
	}
	if err := natsrebuild.ValidateTopicClasses(topics, stateTopics); err != nil {
		logger.Error("invalid topic classification", "err", err)
		os.Exit(2)
	}
	stateSet := make(map[string]bool, len(stateTopics))
	for _, s := range stateTopics {
		stateSet[s] = true
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	natsCfg := bus.NATSConfig{URL: natsURL, Name: "nats-rebuild"}
	if socket != "" {
		src, err := transport.NewSource(ctx, socket)
		if err != nil {
			logger.Error("spiffe source", "err", err)
			os.Exit(1)
		}
		defer func() { _ = src.Close() }()
		natsCfg.TLSConfig = transport.ClientTLSConfig(src, transport.AuthorizeMesh())
		logger.Info("nats-rebuild: mTLS to NATS enabled", "socket", socket)
	} else {
		logger.Warn("nats-rebuild: NATS plaintext (no NATS_REBUILD_SPIFFE_SOCKET)")
	}

	nc, err := bus.DialNATS(ctx, natsCfg)
	if err != nil {
		logger.Error("dial nats", "err", err)
		os.Exit(1)
	}
	defer func() { _ = nc.Close() }()

	now := time.Now()
	var total uint64
	for _, topic := range topics {
		reader, err := replay.NewReader(replay.Config{
			Brokers: brokers,
			Topic:   topic,
			Range:   natsrebuild.WindowFor(topic, stateSet, since, now),
		})
		if err != nil {
			logger.Error("new reader", "topic", topic, "err", err)
			os.Exit(1)
		}
		p := &natsrebuild.Pipeline{Source: reader, Publisher: nc, Logger: logger}
		stats, err := p.Run(ctx)
		_ = reader.Close()
		if err != nil {
			logger.Error("rebuild topic failed", "topic", topic, "published", stats.Published, "err", err)
			os.Exit(1)
		}
		logger.Info("rebuilt topic", "topic", topic, "state", stateSet[topic],
			"published", stats.Published, "malformed", stats.Malformed)
		total += stats.Published
	}
	logger.Info("nats-rebuild complete", "topics", len(topics), "since", since.String(), "published", total)
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

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}
