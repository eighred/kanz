// nats-rebuild reconstructs the NATS live spine from the Kafka log of record
// (DR-01c). Run it once in the DR region after the NATS streams are provisioned
// (the bootstrap-job) and the DR-replicated Kafka log is present (DR-01a): it
// drains a bounded recent window of each log-of-record topic back onto the live
// subjects, then exits. Idempotent re-runs are safe (the stream dedup window +
// idempotent handlers), so a partial rebuild can simply be re-run.
//
// TENANCY: NATS_REBUILD_TOPICS is the archiver's UN-PREFIXED produced set;
// NATS_REBUILD_TENANTS names the tenants to restore and the prefixed Kafka
// topic names are DERIVED from the two (natsrebuild.Qualify, the archiver's own
// rule). Unset ⇒ __system__ only, which is the historical single-tenant run
// unchanged. NATS SUBJECTS ARE NOT PREFIXED — tenancy rides in the envelope,
// not the subject (infra/nats/bootstrap-job.yaml binds market.>, order.> …), so
// a tenant's events republish onto the same live subjects with the envelope's
// tenant_id intact, and no publish-path change is needed.
//
// Exit codes: 2 configuration refused before any I/O; 1 a target failed (a
// topic that does not exist in Kafka is one — see natsrebuild.OutcomeMissing);
// 0 every requested topic existed and was drained.
//
// Kafka is read plaintext over the in-cluster listener (kafka:9092), like the
// provisioning Jobs; NATS publish goes over SEC-01c mTLS when a SPIFFE socket is
// set (required against a verify:true cluster), plaintext otherwise (dev).
package main

import (
	"context"
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/tools/natsrebuild"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	brokers := env.SplitList(env.Or("NATS_REBUILD_KAFKA_BROKERS", "kafka:9092"))
	natsURL := env.Or("NATS_REBUILD_NATS_URL", "nats://nats:4222")
	topics := env.SplitList(os.Getenv("NATS_REBUILD_TOPICS"))
	stateTopics := env.SplitList(os.Getenv("NATS_REBUILD_STATE_TOPICS"))
	socket := os.Getenv("NATS_REBUILD_SPIFFE_SOCKET")
	since, err := time.ParseDuration(env.Or("NATS_REBUILD_SINCE", "24h"))
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
	rawTenants, tenantsSet := os.LookupEnv("NATS_REBUILD_TENANTS")
	tenants, err := natsrebuild.ParseTenants(rawTenants, tenantsSet)
	if err != nil {
		logger.Error("invalid tenant configuration", "err", err)
		os.Exit(2)
	}
	targets, err := natsrebuild.ResolveTargets(tenants, topics, stateTopics)
	if err != nil {
		logger.Error("cannot resolve the topics to rebuild", "err", err)
		os.Exit(2)
	}
	logger.Info("nats-rebuild plan", "tenants", tenants, "base_topics", len(topics), "targets", len(targets))

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

	runner := &natsrebuild.Runner{
		Targets:   targets,
		Requested: tenants,
		Checker:   natsrebuild.KafkaTopicChecker{Brokers: brokers},
		Open:      natsrebuild.KafkaOpen(brokers),
		Publisher: nc,
		Since:     since,
		Now:       time.Now(),
		Logger:    logger,
	}
	report, runErr := runner.Run(ctx)

	// The summary is emitted on BOTH paths, before the exit decision: a failed
	// run's partial account is the operator's only record of what was already
	// republished before the abort, and a re-run needs it.
	logger.Info("nats-rebuild summary",
		"tenants", len(report.Requested), "targets", len(targets), "attempted", len(report.Results),
		"since", since.String(), "published", report.Published(), "per_tenant", report.Summary())
	if barren := report.BarrenTenants(); len(barren) > 0 {
		logger.Warn("tenants that replayed no event at all",
			"tenants", barren,
			"note", "every topic existed but held nothing in the window — check NATS_REBUILD_SINCE "+
				"and that the tenant's archiver has been running")
	}
	if runErr != nil {
		logger.Error("nats-rebuild FAILED — the spine is NOT rebuilt", "err", runErr)
		os.Exit(1)
	}
	logger.Info("nats-rebuild complete", "tenants", report.Requested, "topics", len(targets),
		"since", since.String(), "published", report.Published())
}
