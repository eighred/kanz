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
	"syscall"
	"time"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/web-bff/internal/config"
	"github.com/eighred/kanz/services/web-bff/internal/oidc"
	"github.com/eighred/kanz/services/web-bff/internal/server"
	"github.com/eighred/kanz/services/web-bff/internal/session"
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
		ServiceName:    "web-bff",
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

	oidcClient, err := oidc.New(oidc.Config{
		Issuer:      cfg.Issuer,
		ClientID:    cfg.ClientID,
		RedirectURL: cfg.RedirectURL,
		Scope:       cfg.Scope,
	})
	if err != nil {
		logger.Error("oidc init failed", "err", err)
		return 2
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
		return 2
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
			fatal.Raise(err)
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

	return fatal.Code()
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
