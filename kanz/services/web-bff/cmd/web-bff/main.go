// web-bff is the browser front-of-house over the delivered /v1 edge (PS-02a).
// It runs the OIDC authorization-code + PKCE login against Eighred SSO, holds
// the issued token server-side in a session keyed by an httpOnly cookie, and
// reverse-proxies the browser's /api calls to the api-gateway with that token
// attached. No backend analytics — the CLI's browser counterpart.
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
	"github.com/kanz-eng/kanz/services/web-bff/internal/config"
	"github.com/kanz-eng/kanz/services/web-bff/internal/oidc"
	"github.com/kanz-eng/kanz/services/web-bff/internal/server"
	"github.com/kanz-eng/kanz/services/web-bff/internal/session"
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
		ServiceName:    "web-bff",
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

	oidcClient, err := oidc.New(oidc.Config{
		Issuer:      cfg.Issuer,
		ClientID:    cfg.ClientID,
		RedirectURL: cfg.RedirectURL,
		Scope:       cfg.Scope,
	})
	if err != nil {
		logger.Error("oidc init failed", "err", err)
		os.Exit(2)
	}

	sessions := session.NewManager(cfg.SessionTTL)
	go sweepLoop(ctx, sessions)

	readiness := &server.Readiness{}
	srv, err := server.New(readiness, server.Options{
		OIDC:          oidcClient,
		Sessions:      sessions,
		GatewayURL:    cfg.GatewayURL,
		SecureCookies: cfg.SecureCookies,
		Logger:        logger,
		Metrics:       obs.MetricsHandler(),
	})
	if err != nil {
		logger.Error("server init failed", "err", err)
		os.Exit(2)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("web-bff listening", "addr", cfg.Listen, "issuer", cfg.Issuer, "gateway", cfg.GatewayURL)
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

// sweepLoop drops expired sessions and pending logins periodically so abandoned
// entries do not accumulate in the in-memory store.
func sweepLoop(ctx context.Context, m *session.Manager) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Sweep()
		}
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
