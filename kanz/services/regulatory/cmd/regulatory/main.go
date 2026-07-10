// regulatory filing service entrypoint (WIRE-01e). It serves the delivered
// PARITY-06 filings — FRTB capital, Form PF, AIFMD leverage, TCFD/SFDR climate
// disclosures — assembled from request-supplied inputs, signed, and served
// point-in-time + completeness-gated. It owns no analytics; each endpoint drives
// a delivered File* assembler through the injected Signer.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kanz-eng/kanz/internal/audit/linkstore"
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

	sgnr, closeSigner, err := buildSigner(ctx, cfg, logger)
	if err != nil {
		logger.Error("signer init failed", "err", err)
		os.Exit(2)
	}
	defer closeSigner()

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, sgnr, server.WithMetrics(obs.MetricsHandler())),
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

// buildSigner selects the filing signature backend and owns the durable link
// store's lifecycle. "chain" (default) links every filing into the AUDIT-01
// hash chain via signer.ChainSigner — a signature is then a verifiable chain
// position, not just a standalone digest. Each link is appended to the REG-02
// linkstore.Store (Postgres when REGULATORY_DATABASE_URL is set, else in-memory),
// and the chain head is recovered from the store on startup so the chain
// continues across a restart instead of resetting to Genesis. "hash" is the bare
// content-hash signer (no store). Both satisfy the delivered one-method Signer
// seam. The returned close func tears down the store's pool.
func buildSigner(ctx context.Context, cfg config.Config, logger *slog.Logger) (server.Signer, func(), error) {
	if cfg.Signer == "hash" {
		return regulatory.HashSigner{}, func() {}, nil
	}
	store, closeStore, err := openLinkStore(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	head, err := store.Head(ctx)
	if err != nil {
		closeStore()
		return nil, nil, fmt.Errorf("recover audit chain head: %w", err)
	}
	sgnr := signer.New(head,
		signer.WithSink(func(link signer.Link) error {
			// The sink runs synchronously under the signer lock during a request,
			// but the append is an audit record that must not be dropped if the
			// request is cancelled — bound it to its own short deadline instead.
			appendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return store.Append(appendCtx, link)
		}),
		signer.WithErrorHandler(func(err error) {
			logger.Error("filing chain-link sink failed", "err", err)
		}),
	)
	logger.Info("filing signer ready", "backend", "chain", "durable", cfg.DatabaseURL != "", "resumed", head != "")
	return sgnr, closeStore, nil
}

// openLinkStore selects the durable Postgres link store when a DSN is set
// (REG-02), otherwise the in-memory store. Returns a close func that tears down
// the pool (a no-op for the in-memory store). Both satisfy linkstore.Store.
func openLinkStore(ctx context.Context, cfg config.Config) (linkstore.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		return linkstore.NewMemoryStore(), func() {}, nil
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	store := linkstore.NewPostgres(pool)
	if err := store.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, err
	}
	return store, pool.Close, nil
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
