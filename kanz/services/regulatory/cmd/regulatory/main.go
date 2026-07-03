// regulatory filing service entrypoint (WIRE-01e). It serves the delivered
// PARITY-06 filings — FRTB capital, Form PF, AIFMD leverage, TCFD/SFDR climate
// disclosures — assembled from request-supplied inputs, signed, and served
// point-in-time + completeness-gated. It owns no analytics; each endpoint drives
// a delivered File* assembler through the injected Signer.
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

	"github.com/kanz-eng/kanz/internal/audit/signer"
	"github.com/kanz-eng/kanz/internal/regulatory"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/services/regulatory/internal/config"
	"github.com/kanz-eng/kanz/services/regulatory/internal/server"
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
		ServiceName:    "regulatory",
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
		Handler:           server.New(readiness, logger, buildSigner(cfg, logger), server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("regulatory listening", "addr", cfg.Listen, "signer", cfg.Signer)
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

// buildSigner selects the filing signature backend. "chain" (default) links
// every filing into the AUDIT-01 hash chain via signer.ChainSigner — a signature
// is then a verifiable chain position, not just a standalone digest. The durable
// append of each chain link (to the AUDIT-01 store / a platform.audit FACT) is a
// LinkSink wired here once that producer is composed; until then links chain in
// memory (still verifiable within the process). "hash" is the bare content-hash
// signer. Both satisfy the delivered one-method Signer seam.
func buildSigner(cfg config.Config, logger *slog.Logger) server.Signer {
	if cfg.Signer == "hash" {
		return regulatory.HashSigner{}
	}
	// Start the chain at Genesis; a deployment continuing an existing audit chain
	// passes the current head. WithSink is added when the audit link producer is
	// wired at this composition root.
	return signer.New("", signer.WithErrorHandler(func(err error) {
		logger.Error("filing chain-link sink failed", "err", err)
	}))
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
