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
	// os.Exit skips deferred functions, so it must never run while any resource
	// defer (kafka.Close / nc.Close / obs.Shutdown) is still on the stack — that
	// would mean the crash reason is never flushed via OTel and connections leak.
	// run() owns the whole defer chain and returns a plain exit code; this is the
	// ONLY os.Exit call reachable after a resource has been opened.
	os.Exit(run())
}

// run performs the full archiver lifecycle and returns the process exit code.
// Every defer registered inside it fires before run returns, and only then does
// main call os.Exit — mirroring lake-sink's cmd/lake-sink/main.go, which never
// os.Exits after opening a resource either.
func run() int {
	cfg, err := config.Load()
	if err != nil {
		slog.Default().Error("config load failed", "err", err)
		return 2 // fail closed: a non-archiving archiver reports healthy
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
		return 2
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
		return 2
	}
	defer func() { _ = kafka.Close() }()

	nc, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source})
	if err != nil {
		logger.Error("nats dial failed", "err", err)
		return 2
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

	metrics := archive.NewMetrics(obs.Registry)

	a := archive.New(archive.Config{
		Tenant:   cfg.Tenant,
		Group:    cfg.Group,
		Subjects: cfg.Subjects,
		Kafka:    kafka,
		NATS:     nc,
		Logger:   logger,
		Metrics:  metrics,
		// Ready fires once every subject's Subscribe goroutine has been launched —
		// NOT at construction time (before this call, readiness.Set(true) ran
		// before a single subscription existed, so /readyz reported ready with
		// zero live subscriptions).
		Ready: func() { readiness.Set(true) },
	})

	// Lag poller. The archiver's real failure mode is falling behind its
	// stream's max-age, not going down — and that is invisible on /healthz the
	// whole time it is happening. Poll each subscribed durable's pending count
	// independently of the message path so it keeps reporting even while the
	// archiver itself is stuck (e.g. NACK-looping a produce failure).
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, subject := range cfg.Subjects {
					pending, err := nc.Pending(ctx, subject, cfg.Group)
					if err != nil {
						logger.Warn("archiver: lag poll failed", "subject", subject, "err", err)
						continue
					}
					metrics.Lag.WithLabelValues(subject).Set(float64(pending))
				}
			}
		}
	}()

	runErr := a.Run(ctx)
	readiness.Set(false)
	if runErr != nil {
		logger.Error("archiver stopped", "err", runErr)
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}

	if runErr != nil {
		return 1
	}
	return 0
}

// version is the service version stamped on telemetry. Hardcoded until the build
// injects a git SHA / semver (CICD-01a/c ldflags).
func version() string { return "0.1.0" }
