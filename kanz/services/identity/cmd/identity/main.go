// identity is the platform identity provider (#364): it exchanges a credential
// for a signed token, redeems an invitation into an account, and publishes the
// public half of its signing key at /jwks.json for the api-gateway to verify
// with.
//
// IT HOLDS THE SIGNING KEY AND THE GATEWAY DOES NOT. That split is the point of
// asymmetric issuance: the verifier cannot forge what it verifies. This process
// is not internet-facing — the web-bff reaches it over the private network, and
// nothing else may.
//
// SCHEMA IS NOT APPLIED HERE. cmd/kanz-migrate owns migrations for every service
// in this estate; a service that quietly migrated its own database on boot would
// be a second answer to "who changes the schema", and the one that runs first
// during a rollout would win.
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

	"github.com/eighred/kanz/internal/identity"
	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/identity/internal/config"
	"github.com/eighred/kanz/services/identity/internal/ratelimit"
	"github.com/eighred/kanz/services/identity/internal/server"
	"github.com/eighred/kanz/services/identity/internal/signingkey"
)

func main() {
	// The lifecycle lives in run() because os.Exit skips defers.
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
	fatal := lifecycle.NewFatal(stop)

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    "identity",
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

	// UNSCOPED BY NECESSITY, with the reason travelling to the pool constructor:
	// login must find an account BEFORE it knows the account's tenant, so a
	// tenant-bound pool could only ever find users of the wrong one.
	pool, err := pg.NewGlobalPool(ctx, cfg.DatabaseURL, identity.WhyNoTenantScope)
	if err != nil {
		logger.Error("postgres connect failed", "err", err)
		return 2
	}
	defer pool.Close()

	key, err := signingkey.Load(cfg.SigningKeyFile, cfg.AllowEphemeralKey, logger)
	if err != nil {
		logger.Error("signing key unavailable", "err", err)
		return 2
	}
	signer, err := identity.NewSigner(key, cfg.TokenIssuer, cfg.TokenAudience, cfg.TokenTTL)
	if err != nil {
		logger.Error("token signer init failed", "err", err)
		return 2
	}

	limiter := ratelimit.New(ratelimit.Options{
		Burst:  cfg.LoginBurst,
		Refill: cfg.LoginRefill,
		Logger: logger,
	})
	go sweepLoop(ctx, limiter)

	// AUTHENTICATED PROVISIONING (#364), when a role is named.
	//
	// The VERIFIER IS THE SIGNER. This service mints the tokens the gateway
	// trusts, so checking its own signature is the same answer — reached without a
	// network hop and without a shared secret anyone could forge with. It is also
	// the only option available: /login and /invites/redeem must be reachable by
	// callers holding no token, so this service cannot sit behind the
	// NetworkPolicy that makes X-Kanz-Principal-* trustworthy anywhere else, and
	// a header arriving here is a string the caller typed.
	store := identity.NewPostgres(pool)
	opts := []server.Option{}
	if cfg.OperatorRole != "" {
		// THE AUDIT SINK IS STDOUT, AND THAT IS A CHOICE WITH A COST.
		//
		// auth.SlogRecorder is a real sink and not a stub — kanz logs are stdout
		// JSON shipped by the platform (OBS-01a), so "operator X disabled account
		// Y" reaches the same pipeline every other service's audit lines do. What
		// it does NOT reach is AUDIT-01's append-only projection, which is the
		// tamper-evident system of record: that one is fed off the bus, and THIS
		// SERVICE HAS NO bus.Producer. Giving it one means giving the credential
		// authority a NATS connection and a NetworkPolicy hole it does not have
		// today, which is a larger change than #525 asked for.
		//
		// The upgrade is one line the day that changes — pkg/authbus.NewBusRecorder
		// satisfies the same seam, and statusDecisionLog already stamps the
		// principal.* attributes it partitions and tenants on.
		opts = append(opts, server.WithProvisioning(server.Provisioning{
			Verifier: signer, Store: store, OperatorRole: cfg.OperatorRole, InviteTTL: cfg.InviteTTL,
			Audit: auth.NewSlogRecorder(logger),
		}))
		logger.Info("authenticated provisioning enabled", "operator_role", cfg.OperatorRole,
			"routes", "POST /invites, GET /invites, POST /users/{subject}/disable, "+
				"POST /users/{subject}/enable",
			"audit_sink", "stdout (slog) — NOT the AUDIT-01 projection; this service has no bus")
	} else {
		// WARN, not Info. Without this the ONLY way to create an account is
		// cmd/kanz-invite, which writes to the store directly — so every account on
		// the estate is attributable to whoever held a DSN rather than to a person.
		logger.Warn("authenticated provisioning is DISABLED — the only way to create an account is "+
			"cmd/kanz-invite, which requires the database credential and records the act against "+
			"whoever holds it rather than against a named operator, and THERE IS NO WAY TO DISABLE "+
			"AN ACCOUNT AT ALL: an offboarded trader or a compromised credential can be locked out "+
			"only by a hand-run UPDATE against this database (#525)",
			"enable_with", "IDENTITY_OPERATOR_ROLE")
	}

	srv, err := server.New(store, signer, limiter,
		func() any { return signer.JWKS() }, cfg.TokenIssuer, logger, opts...)
	if err != nil {
		logger.Error("server init failed", "err", err)
		return 2
	}

	mux := http.NewServeMux()
	srv.Routes(mux)
	// Liveness is unconditional; readiness follows the database, because a
	// process that cannot reach its credential store can accept a connection and
	// authenticate nobody — and that must not read as healthy.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(pingCtx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("GET /metrics", obs.MetricsHandler())

	httpSrv := httpserver.New(cfg.Listen, mux, httpserver.Standard())
	go func() {
		logger.Info("identity listening",
			"addr", cfg.Listen, "issuer", cfg.TokenIssuer, "audience", cfg.TokenAudience,
			"kid", signer.KeyID())
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
	return fatal.Code()
}

// sweepLoop drops rate-limiter buckets that carry no information, so a long-
// running process does not accumulate one per attempted subject.
func sweepLoop(ctx context.Context, l *ratelimit.Limiter) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Sweep()
		}
	}
}
