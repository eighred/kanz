// tv-sync is the reverse feedback loop of the automated fund-management system
// (Milestone 2). It folds the OMS order/fill FACT stream into a zero-truth,
// bitemporal, multi-tenant read model and serves it over the TradingView
// Broker-Integration API (REST + streaming), so Eighred traders watch the
// automated funds live on TradingView. It owns no source of truth and issues no
// orders — a pure projection.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/services/tv-sync/internal/brokerapi"
	"github.com/kanz-eng/kanz/services/tv-sync/internal/config"
	"github.com/kanz-eng/kanz/services/tv-sync/internal/projection"
	"github.com/kanz-eng/kanz/services/tv-sync/internal/server"
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
		ServiceName:    "tv-sync",
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

	proj := projection.New(time.Now, nil) // nil marks: unrealized P&L omitted until a mark source is bound

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source})
	if err != nil {
		logger.Error("bus dial failed", "err", err)
		os.Exit(2)
	}
	defer func() { _ = client.Close() }()

	consumer, err := bus.NewConsumer(client)
	if err != nil {
		logger.Error("consumer init failed", "err", err)
		os.Exit(2)
	}

	readiness := &server.Readiness{}
	broker := brokerapi.New(proj)
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, broker, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("tv-sync listening", "addr", cfg.Listen, "nats", cfg.NATSURL)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	// Fold the OMS FACT stream. Each subject is subscribed on its own goroutine
	// (Subscribe blocks); the first non-cancellation error cancels the siblings.
	go func() {
		if err := runConsumers(ctx, consumer, proj, cfg.ConsumerGroup); err != nil && ctx.Err() == nil {
			logger.Error("fact consumer failed", "err", err)
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

func runConsumers(ctx context.Context, consumer *bus.Consumer, proj *projection.Projection, group string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	for _, subject := range projection.Subjects() {
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			if err := consumer.Subscribe(ctx, subject, group, proj.Handle); err != nil && ctx.Err() == nil {
				select {
				case errCh <- err:
					cancel()
				default:
				}
			}
		}(subject)
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		return ctx.Err()
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
