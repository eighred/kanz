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

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source})
	if err != nil {
		logger.Error("bus dial failed", "err", err)
		os.Exit(2)
	}
	defer func() { _ = client.Close() }()

	// No VerifyCommandIssuer: HMAC is this path's perimeter boundary, and the
	// commands issue as "strategy:{id}" (the forged-issuer guard is the gateway's
	// concern for end-user commands). The bus still enforces a non-empty issuer.
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          cfg.Source,
		ProducerVersion: version(),
		Metrics:         bus.NewBusMetrics(obs.Registry),
	})
	if err != nil {
		logger.Error("producer init failed", "err", err)
		os.Exit(2)
	}

	auth := ingest.NewAuthenticator(cfg.Secrets, cfg.Allowlist, cfg.ReplayWindow, time.Now)
	pipeline, err := ingest.NewPipeline(ingest.Options{
		Auth:         auth,
		Symbols:      cfg.Symbols,
		Prices:       cfg.Prices,
		Equity:       cfg.Equity,
		Positions:    ingest.StaticPositions{}, // M1 sim: flat by default; M3+ binds the OMS projection
		Alloc:        cfg.Alloc,
		Publisher:    producer,
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
		Handler:           server.New(readiness, logger, pipeline, server.WithMetrics(obs.MetricsHandler())),
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
