// alternatives (private-markets) binary entrypoint (ALT-01b). It folds commitment
// lifecycle events — capital calls, distributions, NAV marks — into an
// event-sourced fund position and serves position summaries + private-asset
// metrics (IRR/TVPI/DPI/RVPI), the alternative-assets sleeve a whole-portfolio
// view requires (ROI #38). The journal store is in-memory by default; a durable,
// replayable backend and the bus consumer that feeds the journal are wired behind
// the fund.Store seam at the composition root (PERS-01/DEBT-02).
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

	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/services/alternatives/internal/config"
	"github.com/kanz-eng/kanz/services/alternatives/internal/fund"
	"github.com/kanz-eng/kanz/services/alternatives/internal/server"
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
		ServiceName:    "alternatives",
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

	store := fund.NewMemoryStore()
	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, store, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("alternatives listening", "addr", cfg.Listen)
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

// version reads the build's VCS revision for the service-version label.
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
