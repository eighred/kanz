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
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/audit/linkstore"
	"github.com/eighred/kanz/internal/audit/signer"
	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/regulatory"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/services/regulatory/internal/config"
	"github.com/eighred/kanz/services/regulatory/internal/server"
)

// chainDurable reports whether the filing hash chain survives a restart: 1 when
// the link store is Postgres, 0 when it is the in-memory one an operator opted
// into with REGULATORY_ALLOW_EPHEMERAL_CHAIN.
//
// It carries more than durability here. The database is also what OWNS THE CHAIN
// HEAD, and the pg_advisory_xact_lock around AppendChained is what makes the
// shipped replicas: 2 legal — so a 0 on this gauge means the deployment is one
// scale event away from a forked audit chain, not merely a chain that resets on
// restart. Same contract as kanz_audit_log_durable (#236).
//
// The "hash" signer builds no link store and never sets this, which is correct:
// a bare content digest makes no chain claim to be durable about.
var chainDurable = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "kanz_regulatory_chain_durable",
	Help: "1 if the filing hash chain is backed by Postgres (head survives a restart, one head across pods), 0 if in-memory.",
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
		ServiceName:    "regulatory",
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

	// Registered before buildSigner so the posture is on /metrics from the first
	// scrape, including the degraded one.
	obs.Registry.MustRegister(chainDurable)

	sgnr, closeSigner, err := buildSigner(ctx, cfg, logger)
	if err != nil {
		logger.Error("signer init failed", "err", err)
		return 2
	}
	defer closeSigner()

	readiness := &server.Readiness{}
	api := server.New(readiness, logger, sgnr, server.WithMetrics(obs.MetricsHandler()))

	// TWO LISTENERS, AND THE SEPARATION IS THE SECURITY BOUNDARY (#765).
	//
	// allow-observability-scrape selects every pod in kanz-services and admits
	// :8083 across all of them. Until this split that port carried the filing
	// routes, and a filing is not a read — it is signed and appends a link to the
	// AUDIT-01 hash chain — so any pod in the kanz-observability namespace could
	// write to the compliance record, against a service that reads no principal
	// header and therefore has no authentication step to fail.
	//
	// /metrics stays on :8083 because audit already publishes metrics there and
	// the rule admits it anyway; the API moved instead. That is the same trade
	// accounting made in #447: admitting a NEW port for metrics would widen the
	// rule for no gain, so it is the API that leaves.
	metricsMux := http.NewServeMux()
	if h := api.MetricsHandler(); h != nil {
		metricsMux.Handle("GET /metrics", h)
	}
	metricsSrv := httpserver.New(cfg.MetricsListen, metricsMux, httpserver.Standard())
	go func() {
		logger.Info("regulatory metrics listening", "addr", cfg.MetricsListen)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	httpSrv := httpserver.New(cfg.Listen, api, httpserver.Standard())
	go func() {
		logger.Info("regulatory listening", "addr", cfg.Listen, "signer", cfg.Signer)
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
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics shutdown error", "err", err)
	}
	return fatal.Code()
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
	store, closeStore, err := openLinkStore(ctx, cfg, logger)
	if err != nil {
		return nil, nil, err
	}
	// The STORE owns the chain head, not this process.
	//
	// This used to read Head() once at startup and carry it in-process, chaining
	// each filing off the last one THIS pod signed. Two pods would both start from
	// the same head and produce two divergent chains of signature links — a forked
	// audit chain, which is precisely what a regulator would ask about. It is why
	// this service was pinned to a single replica.
	//
	// WithChainer makes every Sign an atomic, advisory-locked read-head→append at
	// the database, serialized across every writer in every pod. There is no head
	// here to fork.
	sgnr := signer.New("",
		signer.WithChainer(store),
		signer.WithErrorHandler(func(err error) {
			logger.Error("filing chain-link append failed — the filing was REFUSED, not issued unsigned", "err", err)
		}),
	)
	// chain_head_owner is DERIVED, not asserted. It read the constant "database"
	// next to `durable=false`, which was wrong on exactly the path that matters:
	// with no DSN the head lives in this process, and saying otherwise at INFO is
	// how a forked chain would have been explained away in an incident.
	headOwner := "database"
	if cfg.DatabaseURL == "" {
		headOwner = "in-process"
	}
	logger.Info("filing signer ready", "backend", "chain", "durable", cfg.DatabaseURL != "", "chain_head_owner", headOwner)
	return sgnr, closeStore, nil
}

// openLinkStore selects the durable Postgres link store when a DSN is set
// (REG-02), and otherwise REFUSES TO START unless the deployment has said out
// loud that it accepts an in-process chain. Returns a close func that tears down
// the pool (a no-op for the in-memory store). Both satisfy linkstore.Store.
//
// This branch used to return linkstore.NewMemoryStore() with no log, no gauge
// and no error (#261), and buildSigner's Info line one frame up reported
// `durable=false` next to `signer ready` — which is a fact stated at INFO in the
// same breath as a success, i.e. exactly the "nothing configured" and "checked,
// and fine" collision CLAUDE.md forbids.
//
// WHY THIS ONE REFUSES, AND WHY IT IS THE SHARPEST OF THE SIX. The manifest's
// scale invariant is built on this store existing. infra/deploy/regulatory-
// deploy.yaml says replicas: 2 is safe "but ONLY because the DATABASE owns the
// chain head", records that this service used to be pinned to one replica
// because an in-process head meant "two pods would both start from the same head
// and produce two divergent chains of signature links -- a forked audit chain,
// which is exactly what a regulator would ask about", and warns: "If it is ever
// removed, this manifest becomes a chain-forking engine -- put the replica count
// back to 1 in the same edit."
//
// An empty DSN removes it. linkstore.AppendChained's guarantee is a
// pg_advisory_xact_lock around read-head → compute → insert; there is no such
// lock in a map, so two pods fork the chain immediately and the store's ON
// CONFLICT (hash) cannot detect it because the forked hashes differ. Nobody
// edited the replica count, so nobody knows. And at one replica the chain simply
// resets to Genesis on every restart, because the head is recovered from this
// store and nowhere else. Neither shape is acceptable, so this is an opt-in.
//
// A "hash" deployment never gets here: buildSigner returns regulatory.HashSigner
// before calling this, and a bare content digest needs no store. That is a
// legitimate DSN-free posture and it stays legitimate.
func openLinkStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (linkstore.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		if !cfg.AllowEphemeralChain {
			return nil, nil, errors.New("no REGULATORY_DATABASE_URL (or _FILE mount) with the chain signer: " +
				"every filing's hash-chain link would be held IN-MEMORY. The chain head is recovered from " +
				"this store and nowhere else, so it RESETS TO GENESIS on each restart — and with the shipped " +
				"replicas: 2 the pods hold separate heads and produce DIVERGENT chains of signature links, a " +
				"forked audit chain no ON CONFLICT can detect. Set REGULATORY_DATABASE_URL, or set " +
				"REGULATORY_SIGNER=hash for bare content digests, or set " +
				"REGULATORY_ALLOW_EPHEMERAL_CHAIN=true to accept it — in which case this deployment MUST run " +
				"exactly one replica and its filings carry no verifiable chain position")
		}
		logger.Warn("FILING HASH CHAIN IS IN-MEMORY — REGULATORY_ALLOW_EPHEMERAL_CHAIN accepted an "+
			"in-process chain. The head RESETS TO GENESIS on every restart, so a filing's chain position is "+
			"verifiable only within the life of this pod. This deployment MUST run exactly ONE replica: two "+
			"pods would fork the audit chain, and the divergence is undetectable because the forked hashes "+
			"differ",
			"fix", "set REGULATORY_DATABASE_URL (or its _FILE mount); the shipped manifest runs replicas: 2",
			"gauge", "kanz_regulatory_chain_durable=0")
		chainDurable.Set(0)
		return linkstore.NewMemoryStore(), func() {}, nil
	}
	pool, err := pg.NewGlobalPool(ctx, cfg.DatabaseURL,
		"the filing link chain is ONE hash chain over the estate's filings — 0001_audit_links.sql "+
			"declares no RLS, and both replicas must read the same head or they fork it. A tenant-scoped "+
			"pool would give each tenant its own genesis, which is the divergence this store exists to "+
			"make impossible")
	if err != nil {
		return nil, nil, err
	}
	store := linkstore.NewPostgres(pool)
	if err := store.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, err
	}
	chainDurable.Set(1)
	return store, pool.Close, nil
}
