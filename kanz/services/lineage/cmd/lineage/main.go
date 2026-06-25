// lineage binary entrypoint (LIN-01). Harvests the event backbone into an
// OpenLineage lineage graph (LIN-01a), serves the catalog + "where did this come
// from" provenance API (LIN-01d), and governs PII lineage access (LIN-01c) via
// the AUTH-01b authorizer with every PII access logged (AUTH-01d). Without
// LINEAGE_NATS_URL it serves the read API only (a read-only replica).
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

	"github.com/kanz-eng/kanz/pkg/auth"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/services/lineage/internal/config"
	"github.com/kanz-eng/kanz/services/lineage/internal/governance"
	"github.com/kanz-eng/kanz/services/lineage/internal/graph"
	"github.com/kanz-eng/kanz/services/lineage/internal/harvest"
	"github.com/kanz-eng/kanz/services/lineage/internal/openlineage"
	"github.com/kanz-eng/kanz/services/lineage/internal/query"
	"github.com/kanz-eng/kanz/services/lineage/internal/server"
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
		ServiceName:    cfg.Source,
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

	g := graph.NewMemory()
	gov, err := buildGovernor(cfg, logger)
	if err != nil {
		logger.Error("governance init failed", "err", err)
		os.Exit(2)
	}
	querySvc := query.NewService(g, gov)

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr: cfg.Listen,
		Handler: server.New(readiness, logger,
			server.WithMetrics(obs.MetricsHandler()),
			server.WithLineage(g, querySvc)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("lineage listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	if cfg.NATSURL != "" {
		if err := runHarvest(ctx, cfg, g, readiness, logger, obs); err != nil {
			logger.Error("harvest stopped with error", "err", err)
		}
	} else {
		readiness.Set(true)
		logger.Warn("no LINEAGE_NATS_URL set — serving the read API only (no harvest)")
		<-ctx.Done()
		readiness.Set(false)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
}

// buildGovernor wires the PII classifier + the AUTH-01b authorizer, decorated so
// every PII access decision is recorded (AUTH-01d access logging). With no
// policy bundle the authorizer denies all PII access — deny-by-default.
func buildGovernor(cfg config.Config, logger *slog.Logger) (*governance.Governor, error) {
	var govCfg *governance.Config
	if cfg.GovernanceFile != "" {
		c, err := governance.LoadConfigFile(cfg.GovernanceFile)
		if err != nil {
			return nil, err
		}
		govCfg = c
	}
	classifier := governance.NewClassifier(govCfg)

	var authorizer auth.Authorizer = denyAll{}
	if cfg.PolicyFile != "" {
		policy, err := auth.LoadPolicyFile(cfg.PolicyFile)
		if err != nil {
			return nil, err
		}
		authorizer = auth.NewPolicyAuthorizer(policy)
	} else {
		logger.Warn("no LINEAGE_POLICY_FILE — PII lineage access denied to all (deny-by-default)")
	}
	// Decorate so every PII decision lands in the observation/log stream.
	audited := auth.NewAuditedAuthorizer(authorizer, auth.NewSlogRecorder(logger), "lineage", logger)
	return governance.NewGovernor(classifier, audited), nil
}

// denyAll is the authorizer used until a policy bundle is mounted: it denies
// every request, so PII lineage is inaccessible by default.
type denyAll struct{}

func (denyAll) Authorize(context.Context, auth.Request) auth.Decision {
	return auth.Decision{Allow: false, Reason: "no policy bundle configured"}
}

// runHarvest subscribes the configured subjects and folds each event into the
// graph + OpenLineage emitter. Mirrors the audit projection: per-subject
// goroutine, first error fails fast.
func runHarvest(ctx context.Context, cfg config.Config, g graph.Graph, readiness *server.Readiness, logger *slog.Logger, obs *observability.Provider) error {
	busMetrics := bus.NewBusMetrics(obs.Registry)

	var emitter openlineage.Emitter = openlineage.NewLogEmitter(logger)
	if cfg.OpenLineageURL != "" {
		emitter = openlineage.NewHTTPEmitter(cfg.OpenLineageURL, nil)
	}
	harvester := harvest.New(g, emitter)

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics))
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
			logger.Info("lineage subscribing", "subject", subject, "group", cfg.ConsumerGroup)
			err := consumer.Subscribe(ctx, subject, cfg.ConsumerGroup, harvester.Handle)
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

// version is the service version stamped on telemetry. Hardcoded until the build
// injects a git SHA / semver (CICD-01a/c ldflags).
func version() string { return "dev" }
