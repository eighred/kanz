// webhook-ingest is the entry point of the automated fund-management loop
// (Milestone 1). It securely receives TradingView Pine webhooks, records each
// as an immutable signal.v1.StrategySignal FACT, resolves the signal's size to
// an absolute quantity against live fund state, and fans it out into N
// order.v1.SubmitOrder COMMANDS on the bus — the commands the OMS already
// executes. It runs no execution logic of its own.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	lifecyclepb "github.com/kanz-eng/kanz-schemas-go/lifecycle/v1"

	"github.com/kanz-eng/kanz/internal/signal/translate"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/services/webhook-ingest/internal/config"
	"github.com/kanz-eng/kanz/services/webhook-ingest/internal/ingest"
	"github.com/kanz-eng/kanz/services/webhook-ingest/internal/server"
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
		ServiceName:    "webhook-ingest",
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

	// The kill-switch, constructed CLOSED. Nothing trades until the lifecycle stream
	// tells us the system is NORMAL, and a lost spine slams it shut again. It is
	// built before the bus so it can be handed to DialNATS as the disconnect
	// watchdog — the gate has to exist before the connection it is watching.
	gate := translate.NewGate(time.Now)

	client, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL:  cfg.NATSURL,
		Name: cfg.Source,
		OnDisconnect: func(err error) {
			gate.TripOnBusLoss(err)
			logger.Error("NATS spine lost — trading halted, operator resume required", "err", err)
		},
		OnReconnect: func() {
			// Deliberately does NOT resume: the wire is back, the book is not
			// necessarily safe. An operator clears the latch.
			_, reason, since := gate.State()
			logger.Warn("NATS spine reconnected — gate remains latched", "reason", reason, "since", since)
		},
	})
	if err != nil {
		logger.Error("bus dial failed", "err", err)
		os.Exit(2)
	}
	defer func() { _ = client.Close() }()

	// One metrics registry for both producer and consumer: NewBusMetrics registers
	// collectors, and registering the same ones twice panics.
	busMetrics := bus.NewBusMetrics(obs.Registry)

	// No VerifyCommandIssuer: HMAC is this path's perimeter boundary, and the
	// commands issue as "strategy:{id}" (the forged-issuer guard is the gateway's
	// concern for end-user commands). The bus still enforces a non-empty issuer.
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          cfg.Source,
		ProducerVersion: version(),
		Metrics:         busMetrics,
	})
	if err != nil {
		logger.Error("producer init failed", "err", err)
		os.Exit(2)
	}

	// The halt FACT stream is what OPENS the gate: it starts closed, and only an
	// operator ModeChanged(system → NORMAL) lets this process trade. If the
	// subscription itself dies, the gate latches shut — a process that cannot hear
	// the brake does not drive.
	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics))
	if err != nil {
		logger.Error("consumer init failed", "err", err)
		os.Exit(2)
	}
	go func() {
		err := consumer.Subscribe(ctx, translate.SubjectModeChanged, cfg.Source+"-halt", gate.Handle)
		if err != nil && ctx.Err() == nil {
			gate.Trip(lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
				"halt subscription failed: "+err.Error())
			logger.Error("halt subscription failed — trading halted", "err", err)
		}
	}()

	auth := ingest.NewAuthenticator(cfg.Secrets, cfg.Allowlist, cfg.ReplayWindow, time.Now)
	pipeline, err := ingest.NewPipeline(ingest.Options{
		Auth:         auth,
		Symbols:      cfg.Symbols,
		Prices:       cfg.Prices,
		Equity:       cfg.Equity,
		Positions:    ingest.StaticPositions{}, // M1 sim: flat by default; M3+ binds the OMS projection
		Alloc:        cfg.Alloc,
		Publisher:    producer,
		Gate:         gate,
		MaxSize:      cfg.MaxSize,
		MaxLeverage:  cfg.MaxLeverage,
		ReplayWindow: cfg.ReplayWindow,
	})
	if err != nil {
		logger.Error("pipeline init failed", "err", err)
		os.Exit(2)
	}

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, pipeline, server.WithMetrics(obs.MetricsHandler()), server.WithCloudflareOnly(cfg.CloudflareOnly)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("webhook-ingest listening", "addr", cfg.Listen, "nats", cfg.NATSURL)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()
	readiness.Set(true)

	<-ctx.Done()
	readiness.Set(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
}

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "dev"
}
