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

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/authbus"
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

// authDecisionsLost counts AUTH-01d decisions that never reached the observation
// stream, by reason.
//
// A COUNTER AND NOT ONLY A LOG LINE, for the reason this whole change exists: a
// decision recorded to stdout is gone at the next rollout. If the recorder
// itself drops one, logging that fact to the same stdout reproduces the defect
// one layer down. `kanz_lineage_auth_decisions_lost_total > 0` is alertable for
// as long as it is true; a WARN scrolls past.
//
// Labelled by reason because the two mean different things to an operator:
// publish_error is the broker refusing or unreachable, queue_full is this
// service producing decisions faster than it can publish them.
var authDecisionsLost = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "kanz_lineage_auth_decisions_lost_total",
	Help: "AUTH-01d authorization decisions that could not be published to the observation stream.",
}, []string{"reason"})

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

	// THE BUS IS DIALLED ONCE, HERE, BECAUSE TWO THINGS NOW NEED IT (#352).
	//
	// It used to be created inside runHarvest, which was correct while the
	// harvest consumer was its only user. The AUTH-01d decision recorder needs a
	// PRODUCER on the same connection, and the governor that holds it is built
	// below — before runHarvest ran at all. Dialling a second connection for the
	// recorder would give one process two client identities on a broker that
	// authenticates per connection (SEC-M3), so the transport moves up instead:
	// one mesh, one client, injected into everything that needs it. That is the
	// shape services/compliance already uses — mesh, client, producer, recorder,
	// consumer, in that order.
	//
	// DEGRADATION IS DELIBERATE AND MUST STAY. With no LINEAGE_NATS_URL this
	// service still serves the read API (see the harvest branch below), so the
	// governor still needs a recorder — it gets the slog one, exactly as before.
	// The bus recorder is used ONLY when a transport exists, which means this
	// change cannot make lineage refuse to start anywhere it starts today.
	//
	// THE BUS SERIES ARE REGISTERED ONLY WHEN THERE IS A BUS. Registering
	// kanz_bus_* and kanz_lineage_auth_decisions_lost_total unconditionally would
	// publish them at zero in a deployment that has no broker at all — and a
	// lost-decisions counter reading 0 says "nothing was lost" when in fact
	// nothing was ever published. Absent means not wired; zero means wired and
	// clean. They must not look the same.
	recorder := auth.DecisionRecorder(auth.NewSlogRecorder(logger))
	var (
		busClient  *bus.NATSClient
		busMetrics *bus.BusMetrics
	)
	if cfg.NATSURL != "" {
		busMetrics = bus.NewBusMetrics(obs.Registry)
		obs.Registry.MustRegister(authDecisionsLost)
		// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is
		// a plaintext client it refuses at the handshake.
		mesh, merr := transport.NewMesh(ctx, cfg.SPIFFESocket)
		if merr != nil {
			logger.Error("bus transport init failed", "err", merr)
			return 2
		}
		defer func() { _ = mesh.Close() }()
		logger.Info("bus transport", "mtls", mesh.Enabled())

		client, cerr := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client})
		if cerr != nil {
			logger.Error("bus dial failed", "err", cerr)
			return 2
		}
		defer func() { _ = client.Close() }()
		busClient = client

		br, rerr := newBusRecorder(client, cfg, busMetrics, logger)
		if rerr != nil {
			logger.Error("decision recorder init failed", "err", rerr)
			return 2
		}
		// DEFER ORDER IS LOAD-BEARING. These run LIFO — br.Close() drains the
		// recorder's queue, THEN client.Close() drops the connection. Registering
		// this before the client's defer would close the connection first and
		// discard whatever audit records were still in flight at shutdown, which
		// is precisely the decisions made just before a rollout.
		defer br.Close()
		recorder = br
		logger.Info("authorization decisions publish to the observation stream")
	} else {
		logger.Warn("no LINEAGE_NATS_URL — AUTH-01d authorization decisions are recorded to the LOG ONLY, " +
			"so they do not survive a restart and cannot be queried beside the FACTs they justified")
	}

	gov, err := buildGovernor(cfg, logger, recorder)
	if err != nil {
		logger.Error("governance init failed", "err", err)
		return 2
	}
	querySvc := query.NewService(g, gov)

	readiness := &server.Readiness{}
	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler()), server.WithLineage(g, querySvc)), httpserver.Standard())
	go func() {
		logger.Info("lineage listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	var runErr error
	if cfg.NATSURL != "" {
		if err := runHarvest(ctx, cfg, g, readiness, logger, obs, busClient, busMetrics); err != nil {
			logger.Error("harvest stopped with error", "err", err)
			runErr = err
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

	// The run loop's own error and any fatal raised from a goroutine answer the
	// same question — why is this process stopping — so they meet here, after
	// every shutdown step above has run. Raise(nil) is a no-op mark.
	fatal.Raise(runErr)
	return fatal.Code()
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
func buildGovernor(cfg config.Config, logger *slog.Logger, recorder auth.DecisionRecorder) (*governance.Governor, error) {
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
	audited := auth.NewAuditedAuthorizer(authorizer, recorder, "lineage", logger)
	return governance.NewGovernor(classifier, audited), nil
}

// newBusRecorder builds the AUTH-01d recorder that publishes decisions onto
// platform.authz.decision.
//
// It is a named function rather than inline wiring so the integration test can
// drive THIS construction instead of a copy of it. That matters more than it
// looks: the defect it exists to hold shut is in the ProducerConfig below, and a
// test that built its own producer would have been green against a service that
// could not publish a single decision.
//
// TENANT IS THE WHOLE PROBLEM HERE. bus.Validate REQUIRES tenant_id on the live
// path, and the producer resolves it as: explicit Event field > ctx > this
// fallback. An authorization decision is raised by an HTTP request to the read
// API — there is NO inbound delivery to inherit a ctx tenant from, which is
// exactly how services/compliance gets away without a fallback (its decisions
// ride a consumer path). Without Tenant set here every publish is rejected as an
// invalid envelope, the error handler counts it, and the feature is 100% broken
// while looking fully wired.
//
// The value is cfg.Tenant (LINEAGE_TENANT), defaulted to bus.SystemTenant by
// config: lineage's graph spans the whole estate rather than one customer, which
// is the second, legitimate meaning of SystemTenant documented in
// pkg/bus/validate.go — the platform's own cross-cutting events, which
// internal/topic maps to the un-prefixed archive topics. A per-customer lineage
// deployment must override it, and that is a deployment review, not something
// Validate can answer.
func newBusRecorder(client *bus.NATSClient, cfg config.Config, busMetrics *bus.BusMetrics, logger *slog.Logger) (*authbus.BusRecorder, error) {
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          cfg.Source,
		ProducerVersion: version.String(),
		Metrics:         busMetrics,
		Tenant:          cfg.Tenant,
	})
	if err != nil {
		return nil, err
	}

	// A DROPPED AUDIT RECORD MUST BE COUNTED, NOT SWALLOWED. BusRecorder
	// publishes in the background off a bounded queue, so both failure modes are
	// silent by default — and an authorization decision that never reaches the
	// observation stream is indistinguishable from one that was never made. That
	// is the same silence this whole issue is about, moved one layer down.
	return authbus.NewBusRecorder(producer,
		authbus.WithErrorHandler(func(err error) {
			authDecisionsLost.WithLabelValues("publish_error").Inc()
			logger.Error("an AUTH-01d decision could not be published — it exists only in this log line",
				"err", err)
		}),
		authbus.WithOverflowHandler(func(*observationpb.DecisionLog) {
			authDecisionsLost.WithLabelValues("queue_full").Inc()
			logger.Error("an AUTH-01d decision was DROPPED because the recorder queue is full — " +
				"the observation stream is now missing decisions the service did make")
		}),
	)
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
func runHarvest(ctx context.Context, cfg config.Config, g graph.Graph, readiness *server.Readiness, logger *slog.Logger, obs *observability.Provider, client *bus.NATSClient, busMetrics *bus.BusMetrics) error {

	var emitter openlineage.Emitter = openlineage.NewLogEmitter(logger)
	if cfg.OpenLineageURL != "" {
		emitter = openlineage.NewHTTPEmitter(cfg.OpenLineageURL, nil)
	}
	harvester := harvest.New(g, emitter)

	// THE TRANSPORT IS NO LONGER CREATED HERE (#352). run() dials it, because the
	// decision recorder needs a producer on the SAME connection and is built
	// before this function runs. One process, one broker identity.

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
