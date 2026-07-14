// compliance binary entrypoint (COMP-01). Runs the post-trade monitor: it keeps
// each portfolio's mandate current from the mandate ConfigChanged stream
// (COMP-01f), re-evaluates books on every position change (COMP-01d), emits a
// FACT-grade ComplianceBreach on a passive breach (feeding AUTO-01), and records
// every decision to the audit stream (COMP-01e). Without COMPLIANCE_NATS_URL it
// serves HTTP/probes only (no consumption).
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

	comp "github.com/kanz-eng/kanz/internal/compliance"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/services/compliance/internal/audit"
	"github.com/kanz-eng/kanz/services/compliance/internal/config"
	"github.com/kanz-eng/kanz/services/compliance/internal/monitor"
	"github.com/kanz-eng/kanz/services/compliance/internal/server"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Default().Error("config load failed", "err", err)
		os.Exit(2)
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

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("compliance listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	if cfg.NATSURL != "" {
		if err := runConsumers(ctx, cfg, readiness, logger, obs); err != nil {
			logger.Error("compliance consumers stopped with error", "err", err)
		}
	} else {
		readiness.Set(true)
		logger.Warn("no COMPLIANCE_NATS_URL set — serving HTTP/probes only (no consumption)")
		<-ctx.Done()
		readiness.Set(false)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
}

// runConsumers wires the bus producer + monitor and subscribes the mandate and
// position streams. The mandate consumer feeds the registry the monitor resolves
// against, so it is subscribed first.
func runConsumers(ctx context.Context, cfg config.Config, readiness *server.Readiness, logger *slog.Logger, obs *observability.Provider) error {
	busMetrics := bus.NewBusMetrics(obs.Registry)

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          cfg.Source,
		ProducerVersion: version(),
		Metrics:         busMetrics,
	})
	if err != nil {
		return err
	}

	// COMP-01f: mandate registry fed from the shared ConfigChanged stream, the
	// point-in-time source the monitor resolves against.
	mandateReg := comp.NewMandateRegistry()
	mandateConsumer := comp.NewMandateConsumer(mandateReg, logger)

	// COMP-01e: decisions to the audit stream. COMP-01d: the post-trade monitor.
	recorder := audit.NewBusRecorder(producer, logger)
	breachEmitter := monitor.NewEmitter(producer)
	mon := monitor.NewMonitor(comp.NewEngine(nil), mandateReg, nil /*classifier*/, breachEmitter, recorder, logger)

	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics))
	if err != nil {
		return err
	}

	type sub struct {
		subject string
		handler bus.EventHandler
	}
	// The mandate registry arms by BROADCAST — see the OMS's comment. A durable group
	// here meant a restarted compliance pod came back with an empty registry and
	// silently governed nothing (EXEC-M13).
	// THE MONITOR ARMS ITSELF AT BOOT, exactly as the mandate registry does (EXEC-M20).
	//
	// Its book was filled by a durable consumer GROUP, which resumes at its last ack — so
	// a restarted monitor came back with an EMPTY book and rebuilt it only as new position
	// FACTs happened to arrive. An instrument that did not trade again was simply gone, and
	// a fund holding an instrument its mandate FORBIDS looked compliant, because the holding
	// was not there to see. The control did not fail; it went blind.
	//
	// SubscribeBroadcast delivers DeliverLastPerSubject over the compacted POSITION stream,
	// so a booting pod learns the CURRENT state of every holding in one read. (This is why
	// the service stays at replicas: 1 — broadcast means every pod folds every position and
	// would emit its own duplicate breach FACT. Lifting that pin is a separate decision.)
	var subs []sub
	for _, s := range cfg.MonitorSubjects() {
		subs = append(subs, sub{s, mon.Handle})
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("compliance arming the mandate registry", "subject", comp.SubjectMandateAll)
		err := consumer.SubscribeBroadcast(ctx, comp.SubjectMandateAll, mandateConsumer.Handle)
		if err != nil && !errors.Is(err, context.Canceled) {
			once.Do(func() {
				firstErr = err
				cancel()
			})
		}
	}()
	for _, s := range subs {
		wg.Add(1)
		go func(s sub) {
			defer wg.Done()
			logger.Info("compliance arming the post-trade book", "subject", s.subject)
			err := consumer.SubscribeBroadcast(ctx, s.subject, s.handler)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(s)
	}
	readiness.Set(true)
	wg.Wait()
	readiness.Set(false)
	return firstErr
}

// version is the service version stamped on telemetry. Hardcoded until the build
// injects a git SHA / semver (CICD-01a/c ldflags).
func version() string { return "dev" }
