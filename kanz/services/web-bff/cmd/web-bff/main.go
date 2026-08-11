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
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/web-bff/internal/clientip"
	"github.com/eighred/kanz/services/web-bff/internal/config"
	"github.com/eighred/kanz/services/web-bff/internal/identityclient"
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

	// THE CREDENTIAL PATH IS THE ONE THAT MUST WORK (#371). The identity service
	// is where a person signs in; OIDC below is the optional alternative for a
	// client bringing their own IdP.
	//
	// The forwarded-address header is the identity service's own — the BFF
	// resolves the browser's address here and passes it on, because a login
	// relayed by this process would otherwise arrive from this process, and the
	// identity limiter would key every attempt in the estate together.
	identityClient := identityclient.New(cfg.IdentityURL, identityForwardHeader, 0)

	ipResolver, err := clientip.NewResolver(cfg.TrustedProxyHeader, cfg.TrustedProxies)
	if err != nil {
		logger.Error("trusted proxy configuration invalid", "err", err)
		return 2
	}
	// SAID OUT LOUD, because "configured and ignored" and "not configured" must
	// not look the same. Behind the tunnel every request shares one peer, so a
	// resolver that trusts nothing makes the login limiter global.
	if ipResolver.Trusts() {
		logger.Info("client address resolved from the edge header",
			"header", cfg.TrustedProxyHeader, "trusted_proxies", cfg.TrustedProxies)
	} else {
		logger.Warn("no trusted proxy configured — every caller is attributed to its immediate " +
			"peer. Behind an edge that is ONE address for everyone, so the identity service's " +
			"login rate limit becomes estate-wide rather than per-caller.")
	}

	// OPTIONAL. Absent an issuer the OIDC routes are not registered at all,
	// rather than registered and answering a browser with a redirect to nowhere.
	var oidcClient *oidc.Client
	if cfg.Issuer != "" {
		oidcClient, err = oidc.New(oidc.Config{
			Issuer:      cfg.Issuer,
			ClientID:    cfg.ClientID,
			RedirectURL: cfg.RedirectURL,
			Scope:       cfg.Scope,
		})
		if err != nil {
			logger.Error("oidc init failed", "err", err)
			return 2
		}
		logger.Info("OIDC login enabled alongside credential login", "issuer", cfg.Issuer)
	}

	sessions := session.NewManager(cfg.SessionTTL)
	go sweepLoop(ctx, sessions)

	readiness := &server.Readiness{}
	srv, err := server.New(readiness, server.Options{
		Identity:      identityClient,
		ClientIP:      ipResolver,
		StaticDir:     cfg.StaticDir,
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

	httpSrv := httpserver.New(cfg.Listen, srv, httpserver.Standard())
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

// identityForwardHeader is the header the identity service reads a caller's
// address from. It is this repo's own name rather than a vendor one: the BFF
// talks to identity over the private network, so there is no edge in between to
// set CF-Connecting-IP, and reusing that name would invite someone to trust it
// there too.
const identityForwardHeader = "X-Kanz-Client-IP"
