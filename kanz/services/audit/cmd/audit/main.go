// audit binary entrypoint (AUDIT-01). Consumes the event backbone and
// materializes every decision, command, outcome, data-quality event, and
// AUTH-01d authz decision into an append-only, hash-chained, queryable audit log
// (AUDIT-01a/b), and serves the lineage (AUDIT-01c) + reporting (AUDIT-01d) API.
// With AUDIT_DATABASE_URL set the store is the durable WORM Postgres log;
// without it the store is in-memory (local/dev only — an audit log that doesn't
// survive a restart is not an audit log), and openStore REFUSES TO START unless
// AUDIT_ALLOW_EPHEMERAL_LOG=true says the deployment accepts that.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/audit/internal/audit"
	"github.com/eighred/kanz/services/audit/internal/config"
	"github.com/eighred/kanz/services/audit/internal/server"
)

// auditLogDurable reports whether the tamper-evidence log survives a restart:
// 1 when it is the WORM Postgres store, 0 when it is the in-memory one an
// operator opted into with AUDIT_ALLOW_EPHEMERAL_LOG.
//
// The gauge is what makes the degraded posture ALERTABLE rather than merely
// readable. A start-up WARN scrolls off; `kanz_audit_log_durable == 0` can be
// alerted on for as long as it is true, which matters here more than anywhere
// else in the estate: nothing downstream of audit ever notices that the log is
// ephemeral, so the first symptom is a restart that has already happened and a
// hash chain that is already gone. Same shape, and deliberately the same
// contract, as kanz_venue_orderview_durable (INFRA-M7a-2).
var auditLogDurable = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "kanz_audit_log_durable",
	Help: "1 if the audit log is backed by the WORM Postgres store (survives a restart), 0 if in-memory.",
})

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
		ServiceName:    cfg.Source,
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

	// Registered before openStore so the posture is on /metrics from the first
	// scrape, including the degraded one. A gauge nobody registered is a Set
	// call into the void — exactly the silence this whole branch is about.
	obs.Registry.MustRegister(auditLogDurable)

	store, closeStore, err := openStore(ctx, cfg, logger)
	if err != nil {
		logger.Error("store open failed", "err", err)
		return 2
	}
	defer closeStore()
	if err := store.Ping(ctx); err != nil {
		logger.Error("store ping failed", "err", err)
		return 2
	}

	// WHO MAY VERIFY THE CHAIN IS A DEPLOYMENT DECISION, AND IT MUST BE STATED
	// (#118). /v1/audit/verify is the one read here that is deliberately NOT
	// tenant-scoped — the chain is one sequence across every tenant — so its
	// attestation carries an estate-wide record count. Unset roles and a
	// deliberately-open deployment would otherwise start identically, and the
	// silent one leaks that count to every tenant.
	//
	// Same shape as the AUDIT_DATABASE_URL refusal below it, for the same reason:
	// the deployment that FORGOT must not look like the deployment that MEANT it.
	if len(cfg.VerifyRoles) == 0 && !cfg.AllowUnrestrictedVerify {
		logger.Error("no AUDIT_VERIFY_ROLES: GET /v1/audit/verify returns an attestation over EVERY " +
			"tenant's records, so its count tells any authenticated caller how much other tenants' " +
			"activity this platform carries. Name the operator/monitoring roles that may read it, or set " +
			"AUDIT_ALLOW_UNRESTRICTED_VERIFY=true to accept that any authenticated principal may — in " +
			"which case the estate-wide count is not confidential in this deployment")
		return 2
	}
	if len(cfg.VerifyRoles) == 0 {
		logger.Warn("CHAIN VERIFICATION IS OPEN TO ANY AUTHENTICATED PRINCIPAL — "+
			"AUDIT_ALLOW_UNRESTRICTED_VERIFY accepted an estate-wide attestation readable by every "+
			"tenant. The record count discloses how much other tenants' activity this platform carries",
			"fix", "set AUDIT_VERIFY_ROLES to the operator and monitoring roles")
	}

	readiness := &server.Readiness{}
	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger,
		server.WithMetrics(obs.MetricsHandler()), server.WithStore(store),
		server.WithVerifyRoles(cfg.VerifyRoles)), httpserver.Standard())
	go func() {
		logger.Info("audit listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	var runErr error
	if cfg.NATSURL != "" {
		if err := runProjection(ctx, cfg, store, readiness, logger, obs); err != nil {
			logger.Error("projection stopped with error", "err", err)
			runErr = err
		}
	} else {
		readiness.Set(true)
		logger.Warn("no AUDIT_NATS_URL set — serving the read API only (no projection)")
		<-ctx.Done()
		readiness.Set(false)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}

	// The run loop's own error and any fatal raised from a goroutine answer the
	// same question — why is this process stopping — so they meet here, after
	// every shutdown step above has run. Raise(nil) is a no-op mark.
	fatal.Raise(runErr)
	return fatal.Code()
}

// runProjection subscribes the configured subjects and folds each event into the
// audit store. Each subject runs on its own goroutine (Subscribe blocks); the
// first non-cancellation error cancels the siblings and is returned — fail-fast,
// so a broken subscription brings the projection down rather than silently
// leaving an audit gap.
func runProjection(ctx context.Context, cfg config.Config, store audit.Store, readiness *server.Readiness, logger *slog.Logger, obs *observability.Provider) error {
	busMetrics := bus.NewBusMetrics(obs.Registry)
	projector := audit.NewProjector(store, time.Now)

	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return err
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client, Metrics: busMetrics})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics), bus.WithDLQ(client))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for _, subject := range cfg.Subjects {
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			logger.Info("audit subscribing", "subject", subject, "group", cfg.ConsumerGroup)
			err := consumer.Subscribe(ctx, subject, cfg.ConsumerGroup, projector.Handle)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(subject)
	}
	readiness.Set(true)
	wg.Wait()
	readiness.Set(false)
	return firstErr
}

// openStore selects the durable Postgres WORM store when a DSN is set, and
// otherwise REFUSES TO START unless the deployment has said out loud that it
// accepts an ephemeral log. Returns a close func (a no-op for in-memory).
//
// This branch used to return audit.NewMemory() with no log, no metric and no
// gauge. The package doc has always stated the consequence — "an audit log that
// doesn't survive a restart is not an audit log" — and the running process said
// nothing at all: a clean start, /readyz 200, queries answered, and the entire
// hash-chained tamper-evidence record discarded by the next rollout. Nothing
// downstream of audit consumes the audit log, so there is no second observer to
// notice; the first symptom is a compliance request that cannot be answered
// about a window nobody knew was missing.
//
// WHY THIS ONE REFUSES WHERE ITS PEERS WARN. The OMS and the venue adapters warn
// and carry on because their in-memory fallback is CORRECT at exactly one
// replica — a real, if degraded, operating posture with a stated precondition.
// There is no equivalent for this store: no replica count, no traffic level and
// no deployment shape makes a RAM-resident audit log acceptable in production.
// A degradation that is never right is an opt-in, not a default — the same call
// webhook-ingest's nonce store makes for the same reason (nonces_default.go).
//
// By the time openStore runs, config.Load has already refused to start if the
// _FILE mount was DECLARED but unreadable (secret.Read), so an empty DSN here
// can only mean no DSN was ever configured — which is precisely the case that
// must not be silent. The shipped manifest always mounts one
// (infra/deploy/audit-deploy.yaml), so this refusal does not change the
// deployed posture; it changes what happens to the deployment that forgot.
func openStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (audit.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		if !cfg.AllowEphemeralLog {
			return nil, nil, errors.New("no AUDIT_DATABASE_URL (or _FILE mount): the audit log would be " +
				"IN-MEMORY and would be DISCARDED on the next restart, rollout or eviction — hash chain, " +
				"lineage and every AUTH-01d authz decision with it. This is the compliance record, and " +
				"nothing downstream of it would report the loss. Set AUDIT_DATABASE_URL, or set " +
				"AUDIT_ALLOW_EPHEMERAL_LOG=true to accept an audit log that does not survive a restart " +
				"— in which case this deployment is NOT a system of record")
		}
		logger.Warn("AUDIT LOG IS IN-MEMORY — AUDIT_ALLOW_EPHEMERAL_LOG accepted an ephemeral compliance "+
			"record. The hash chain, the lineage and every recorded authz decision are DISCARDED on the "+
			"next restart, rollout or eviction, and nothing downstream reports the loss: this deployment "+
			"is not a system of record",
			"fix", "set AUDIT_DATABASE_URL (or its _FILE mount)",
			"gauge", "kanz_audit_log_durable=0")
		auditLogDurable.Set(0)
		return audit.NewMemory(), func() {}, nil
	}
	pool, err := pg.NewGlobalPool(ctx, cfg.DatabaseURL,
		"the audit log is the ESTATE's tamper-evidence record, not a tenant's: 0001_audit_log.sql "+
			"declares no RLS and the hash chain is one sequence over every tenant's events, so a "+
			"tenant-scoped pool would fork the chain per tenant and destroy the property the log exists for")
	if err != nil {
		return nil, nil, err
	}
	auditLogDurable.Set(1)
	return audit.NewPostgres(pool), pool.Close, nil
}
