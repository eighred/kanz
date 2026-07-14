// archiver binary entrypoint (DATA-M1). Drains the NATS live spine (EVT-08) into
// the Kafka durable log of record (EVT-09) — the producer that, until this shipped,
// did not exist, leaving the log of record empty and every event older than the
// stream's max-age gone forever.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/services/archiver/internal/archive"
	"github.com/kanz-eng/kanz/services/archiver/internal/config"
	"github.com/kanz-eng/kanz/services/archiver/internal/server"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Default().Error("config load failed", "err", err)
		os.Exit(2) // fail closed: a non-archiving archiver reports healthy
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    cfg.Source,
		ServiceVersion: version(),
		OTLPEndpoint:   cfg.OTLPEndpoint,
		SampleRatio:    1,
	}, base)
	if err != nil {
		slog.Default().Error("observability init failed", "err", err)
		os.Exit(2)
	}
	logger := obs.Logger
	slog.SetDefault(logger)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = obs.Shutdown(shutCtx)
	}()

	kafka, err := bus.DialKafka(bus.KafkaConfig{Brokers: cfg.Brokers, ClientID: cfg.Source})
	if err != nil {
		logger.Error("kafka dial failed", "err", err)
		os.Exit(2)
	}
	defer func() { _ = kafka.Close() }()

	nc, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source})
	if err != nil {
		logger.Error("nats dial failed", "err", err)
		os.Exit(2)
	}
	defer func() { _ = nc.Close() }()

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("archiver listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	a := archive.New(archive.Config{
		Tenant:   cfg.Tenant,
		Group:    cfg.Group,
		Subjects: cfg.Subjects,
		Kafka:    kafka,
		NATS:     nc,
		Logger:   logger,
	})
	readiness.Set(true)

	if err := a.Run(ctx); err != nil {
		logger.Error("archiver stopped", "err", err)
		os.Exit(1)
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
}

// version is the service version stamped on telemetry. Hardcoded until the build
// injects a git SHA / semver (CICD-01a/c ldflags).
func version() string { return "0.1.0" }
