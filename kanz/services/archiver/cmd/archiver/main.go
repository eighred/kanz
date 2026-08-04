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

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/archiver/internal/archive"
	"github.com/eighred/kanz/services/archiver/internal/config"
	"github.com/eighred/kanz/services/archiver/internal/server"
)

func main() {
	// The lifecycle lives in run() because os.Exit skips defers: every defer
	// run() registers fires before this line. The non-zero code is what makes a
	// fatal halt distinguishable from a graceful SIGTERM — both otherwise exit 0
	// with reason "Completed" in the pod's termination record (#266).
	// 2 = startup failure, 1 = run loop died after startup, 0 = clean shutdown.
	os.Exit(run())
}

// run performs the full archiver lifecycle and returns the process exit code.
// Every defer registered inside it (kafka.Close / mesh.Close / nc.Close, and the
// obs.Shutdown that flushes the crash reason via OTel) fires before run returns,
// and only then does main call os.Exit.
//
// This comment used to justify the shape by saying it mirrored lake-sink, "which
// never os.Exits after opening a resource either." That premise was never true in
// the sense it implied: lake-sink did not os.Exit after opening a resource because
// it never exited non-zero AT ALL — the property being cited was the #266 bug, not
// a pattern. Both are now the same shape for the reason above, and
// test/arch/exit_code_delegation_test.go is what keeps them there.
func run() int {
	cfg, err := config.Load()
	if err != nil {
		slog.Default().Error("config load failed", "err", err)
		return 2 // fail closed: a non-archiving archiver reports healthy
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Records WHY we are stopping so the exit code can say so. Every site that
	// calls fatal.Raise below already brought the process down with stop(); the
	// only thing added is that the reason survives to the exit status (#266).
	fatal := lifecycle.NewFatal(stop)

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    cfg.Source,
		ServiceVersion: version.String(),
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

	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		logger.Error("spiffe source init failed", "err", err)
		return 2
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())

	nc, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client})
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
			fatal.Raise(err)
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

	// The run loop's own error and any fatal raised from a goroutine answer the
	// same question — why is this process stopping — so they meet here, after
	// every shutdown step above has run. Raise(nil) is a no-op mark.
	fatal.Raise(runErr)
	return fatal.Code()
}
