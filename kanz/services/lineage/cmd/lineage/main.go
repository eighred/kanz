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

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/lineage/internal/config"
	"github.com/eighred/kanz/services/lineage/internal/governance"
	"github.com/eighred/kanz/services/lineage/internal/graph"
	"github.com/eighred/kanz/services/lineage/internal/harvest"
	"github.com/eighred/kanz/services/lineage/internal/openlineage"
	"github.com/eighred/kanz/services/lineage/internal/query"
	"github.com/eighred/kanz/services/lineage/internal/server"
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
		ServiceVersion: version.String(),
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

	// THE EVENT INDEX IS BOUNDED, AND THE ANSWERS SAY SO (#244).
	//
	// This subscribes to ">", so the event_id → dataset index grew with the whole
	// estate's event rate and nothing ever removed an entry: tens of millions of
	// map entries a day, then an OOMKill, then a pod back in its Service in
	// milliseconds with an empty graph that the durable consumer will never
	// rebuild — answering "no provenance" for every trade that preceded it.
	//
	// WithCompleteHistory is deliberately NOT set. The consumer below resumes at
	// last ack, so this index covers "since this pod started", and every miss must
	// come back as unknown rather than as a finding. Setting it here would restore
	// exactly the lie the cap would otherwise have made worse.
	g := graph.NewMemory(graph.WithEventIndexCapacity(cfg.EventIndexMax))
	logger.Info("lineage event index bounded", "capacity", cfg.EventIndexMax,
		"note", "misses outside the retained window answer 410 unknown, not 404 not-found")
	registerCoverageMetrics(obs.Registry, g)

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

// registerCoverageMetrics exposes what the bounded index is holding and losing.
//
// GaugeFuncs reading through graph.Coverage, the same stance as the OMS mark
// fold (#96): a level, not an event, and read from the structure it describes so
// the metric cannot drift from it. Without these, "is the cap right for this
// estate" is answerable only by watching RSS, which is the measurement that was
// missing when the map was unbounded.
func registerCoverageMetrics(reg prometheus.Registerer, g graph.Graph) {
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_lineage_event_index_retained",
		Help: "Event ids currently indexed. Sitting at kanz_lineage_event_index_capacity means " +
			"eviction is live and provenance older than the retained window answers unknown.",
	}, func() float64 { return float64(g.Coverage().Retained) }))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_lineage_event_index_capacity",
		Help: "Hard ceiling on indexed event ids (LINEAGE_EVENT_INDEX_MAX).",
	}, func() float64 { return float64(g.Coverage().Capacity) }))
	reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "kanz_lineage_event_index_evicted_total",
		Help: "Event ids dropped to stay under the cap, for this process. Non-zero means no miss " +
			"on this pod is evidence that an event was never observed.",
	}, func() float64 { return float64(g.Coverage().Evicted) }))
	reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "kanz_lineage_unlinked_causes_total",
		Help: "Events whose causation id was not indexed when they arrived, so the derived-from edge " +
			"was never drawn. Sustained growth once the index is full means the cap is below the " +
			"estate's causation latency and upstream answers are silently short.",
	}, func() float64 { return float64(g.Coverage().UnlinkedCauses) }))
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

	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return err
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client})
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
