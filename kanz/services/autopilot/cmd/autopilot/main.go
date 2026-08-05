// autopilot binary entrypoint (AUTO-01). The closed-loop ops controller:
// consumes the operational signal stream (DATA-07 quality events, DATA-05
// reconciliation divergence, model drift, SLO burn, inference circuit state),
// matches each to a recognized condition, runs its remediation runbook
// (quarantine / model-rollback / scale / failover), and escalates to a human on
// anything novel (AUTO-01a–d). Without AUTOPILOT_NATS_URL it serves probes only.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/autopilot/internal/actuate"
	"github.com/eighred/kanz/services/autopilot/internal/config"
	"github.com/eighred/kanz/services/autopilot/internal/controller"
	"github.com/eighred/kanz/services/autopilot/internal/plan"
	"github.com/eighred/kanz/services/autopilot/internal/remediate"
	"github.com/eighred/kanz/services/autopilot/internal/server"
)

func main() {
	// The lifecycle lives in run() because os.Exit skips defers: every defer
	// run() registers fires before this line. The non-zero code is what makes a
	// fatal halt distinguishable from a graceful SIGTERM — both otherwise exit 0
	// with reason "Completed" in the pod's termination record (#266).
	// 2 = startup failure, 1 = run loop died after startup, 0 = clean shutdown.
	os.Exit(run())
}

func run() int {
	cfg, err := config.Load()
	if err != nil {
		slog.Default().Error("config load failed", "err", err)
		return 2
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

	readiness := &server.Readiness{}
	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler())), httpserver.Standard())
	go func() {
		logger.Info("autopilot listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	var runErr error
	if cfg.NATSURL != "" {
		if err := runControlLoop(ctx, cfg, readiness, logger, obs); err != nil {
			logger.Error("control loop stopped with error", "err", err)
			runErr = err
		}
	} else {
		readiness.Set(true)
		logger.Warn("no AUTOPILOT_NATS_URL set — serving probes only (no control loop)")
		<-ctx.Done()
		readiness.Set(false)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}

	// The run loop's own error and any fatal raised from a goroutine answer the
	// same question — why is this process stopping — so they meet here, after
	// every shutdown step above has run. Raise(nil) is a no-op mark.
	fatal.Raise(runErr)
	return fatal.Code()
}

// runControlLoop wires the default remediators/actuators (log-by-default; a
// deployment swaps in command-publishing / k8s impls behind the seams) into the
// control policy and subscribes the signal subjects. Per-subject goroutine,
// first error fails fast (the audit/lineage projection pattern).
func runControlLoop(ctx context.Context, cfg config.Config, readiness *server.Readiness, logger *slog.Logger, obs *observability.Provider) error {
	busMetrics := bus.NewBusMetrics(obs.Registry)

	deps := plan.Deps{
		Quarantiner: remediate.NewLogQuarantiner(logger),
		ModelRoller: remediate.NewLogModelRoller(logger),
		Scaler:      actuate.NewLogScaler(logger),
		Failover:    actuate.NewLogFailover(logger),
	}
	ctrl := controller.New(
		plan.DefaultMatcher(),
		plan.DefaultRegistry(deps, cfg.AutoFailover),
		controller.NewLogEscalator(logger),
		logger,
		controller.NewMetrics(obs.Registry),
	)

	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return err
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics), bus.WithDLQ(client))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for _, subject := range cfg.Subjects {
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			logger.Info("autopilot subscribing", "subject", subject, "group", cfg.ConsumerGroup)
			err := consumer.Subscribe(ctx, subject, cfg.ConsumerGroup, ctrl.Handle)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(subject)
	}
	readiness.Set(true)
	wg.Wait()
	readiness.Set(false)
	return firstErr
}
