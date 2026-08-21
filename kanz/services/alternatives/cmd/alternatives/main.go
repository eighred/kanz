// alternatives (private-markets) binary entrypoint (ALT-01b). It folds commitment
// lifecycle events — capital calls, distributions, NAV marks — into an
// event-sourced fund position and serves position summaries + private-asset
// metrics (IRR/TVPI/DPI/RVPI), the alternative-assets sleeve a whole-portfolio
// view requires (ROI #38). With ALTERNATIVES_DATABASE_URL set the journal is the
// durable Postgres store; without it openStore REFUSES TO START unless
// ALTERNATIVES_ALLOW_EPHEMERAL_JOURNAL=true says the deployment accepts losing
// every cashflow on restart (#261).
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
	"github.com/eighred/kanz/services/alternatives/internal/config"
	"github.com/eighred/kanz/services/alternatives/internal/consume"
	"github.com/eighred/kanz/services/alternatives/internal/fund"
	"github.com/eighred/kanz/services/alternatives/internal/server"
)

// journalDurable reports whether the commitment journal survives a restart: 1
// when it is the Postgres store, 0 when it is the in-memory one an operator
// opted into with ALTERNATIVES_ALLOW_EPHEMERAL_JOURNAL. The gauge outlives the
// start-up WARN, which is what makes the degraded posture alertable. Same
// contract as kanz_audit_log_durable (#236).
var journalDurable = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "kanz_alternatives_journal_durable",
	Help: "1 if the commitment journal is backed by Postgres (survives a restart), 0 if in-memory.",
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
		ServiceName:    "alternatives",
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

	// SEC-M3: one workload identity for the process, shared by the consumer's
	// bus dial. The production broker requires a client SVID; a nil TLSConfig is
	// a plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		logger.Error("spiffe source init failed", "err", err)
		return 2
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())

	// Registered before openStore so the posture is on /metrics from the first
	// scrape, including the degraded one.
	obs.Registry.MustRegister(journalDurable)

	store, closeStore, err := openStore(ctx, cfg, logger)
	if err != nil {
		logger.Error("store init failed", "err", err)
		return 2
	}
	defer closeStore()

	readiness := &server.Readiness{}
	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger, store, server.WithMetrics(obs.MetricsHandler())), httpserver.Standard())
	go func() {
		logger.Info("alternatives listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	// Commitment-lifecycle consumer (ALT-01b): fold live capital-call,
	// distribution and NAV-mark FACTs into the same fund.Store the server reads,
	// so position summaries and IRR/TVPI/DPI/RVPI reflect live activity. Runs
	// concurrently with the server; both share ctx so SIGTERM stops them
	// together. Empty ALTERNATIVES_NATS_URL ⇒ read-only (default), the same
	// stance accounting takes for its fill consumer.
	var consumers sync.WaitGroup
	if cfg.NATSURL != "" {
		consumers.Add(1)
		go func() {
			defer consumers.Done()
			if err := runConsumer(ctx, cfg, store, mesh, logger, obs); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("commitment consumer stopped with error", "err", err)
				fatal.Raise(err)
			}
		}()
	} else {
		logger.Info("no ALTERNATIVES_NATS_URL set — serving read endpoints only (no lifecycle folding)")
	}
	readiness.Set(true)

	<-ctx.Done()
	readiness.Set(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}

	awaitConsumers(&consumers, logger)

	return fatal.Code()
}

// awaitConsumers blocks until the commitment-lifecycle consumer has finished
// draining. It is the LAST statement in main for a reason: everything that
// tears this process down — closeStore (pool.Close), mesh.Close, obs.Shutdown —
// is deferred, so it runs after main's body and cannot be reordered ahead of
// this wait.
//
// #233: without it, main returned from <-ctx.Done(), ran a near-instant
// httpSrv.Shutdown, and fired those defers while the consumer was still
// folding. pkg/bus does not STOP a cancelled consumer, it DRAINS one —
// Subscribe deliberately spends up to DrainGrace + drainStopSlack (5s + 2s,
// pkg/bus/nats.go) after ctx.Done() handing buffered messages to the handler,
// because Stop() would abandon them and reorder the replayed log on every
// rolling deploy. Those handler calls landed on a closed pool, fund.Postgres
// Append failed, and bus.WithDLQ republished each capital call, distribution
// and NAV mark to dlq.<subject> and ACKED it. tools/replay refuses dlq.
// subjects, so those events were gone from the append-only journal the IRR/TVPI
// numbers are derived from, with no supplied way back — on every deploy.
//
// THE BOUND IS A context.WithTimeout(context.Background(), N*time.Second) ON
// PURPOSE, not a bare time.After: that is the exact spelling
// test/arch/pod_disruption_and_drain_test.go's worstServiceShutdownSeconds
// greps for to derive every manifest's terminationGracePeriodSeconds from the
// source. Rewriting it as a timer would drop 10s out of the derived drain
// budget while the process still spends it, and nothing would say so.
//
// 10s against a 7s bus drain: enough headroom for the drain plus the consumer's
// own teardown, short enough to stay inside the grace period the manifests set.
// That grace period is NOT quoted here — the version of this line that quoted
// 45s was already wrong (#264), and the arch guard is where it is computed. When
// the budget expires we close anyway and say so loudly: a handler that ignores
// context cancellation must not hold the pod until the kubelet SIGKILLs it.
//
// Copied, not shared: services/risk-engine/internal/app/lifecycle.go is the
// reference implementation and the right home for this once promoted — the day
// it moves, TestNoUndeclaredShutdownBudgetReachableFromAMain keeps it counted.
func awaitConsumers(consumers *sync.WaitGroup, logger *slog.Logger) {
	joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer joinCancel()

	drained := make(chan struct{})
	go func() {
		consumers.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		logger.Info("bus consumers drained")
	case <-joinCtx.Done():
		logger.Error("bus consumers did not finish draining within the join budget — closing the " +
			"commitment journal and bus client anyway; any event still in a handler will fail its " +
			"Append and be routed to dlq.<subject>")
	}
}

// openStore selects the durable Postgres commitment journal when a DSN is set,
// and otherwise REFUSES TO START unless the deployment has said out loud that it
// accepts an ephemeral journal. Returns a close func that tears down the pool (a
// no-op for the in-memory store). Both satisfy fund.Store, so the server and the
// fold are identical either way.
//
// fund.Postgres has existed since PARITY-02b, tested and unused: nothing ever
// constructed it, so the append-only journal this service is built around lived
// in a map and vanished on restart — silently, which is #261.
//
// WHY THIS ONE REFUSES. A capital call is a legal obligation the fund has
// entered into, and the journal is append-only BECAUSE the metrics need every
// entry: IRR and TVPI are computed over the whole dated cashflow series, not
// over a latest value. That is exactly why infra/nats/bootstrap-job.yaml gives
// ALTERNATIVES a plain 168h stream while WEALTH gets a compacted one — the
// bootstrap comment says so and warns that compacting this stream "would
// silently discard every cashflow but the last per fund". An in-memory journal
// does something strictly worse: it discards ALL of them, and the fold cannot be
// rebuilt, because the consumer group resumes at its last ack and a commitment
// made last month is long past the stream's 168h horizon. The service would come
// back reporting an IRR computed over nothing and call it a number.
//
// No replica count helps: infra/deploy/alternatives-deploy.yaml runs replicas: 2,
// and two maps mean two partial cashflow series answering the same query
// differently. So this is an opt-in, not a default — the audit / webhook-ingest
// call, for the same reason.
//
// config.Load has already refused to start if the _FILE mount was DECLARED but
// unreadable (secret.Read), so an empty DSN here can only mean none was ever
// configured. The shipped manifest always mounts one.
func openStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (fund.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		if !cfg.AllowEphemeralJournal {
			return nil, nil, errors.New("no ALTERNATIVES_DATABASE_URL (or _FILE mount): the commitment " +
				"journal would be IN-MEMORY and every capital call, distribution and NAV mark would be " +
				"DISCARDED on the next restart, rollout or eviction — and IRR/TVPI would then be reported " +
				"over whatever remained. The fold cannot be rebuilt: the consumer group resumes at its last " +
				"ack and the ALTERNATIVES stream ages out at 168h. Set ALTERNATIVES_DATABASE_URL, or set " +
				"ALTERNATIVES_ALLOW_EPHEMERAL_JOURNAL=true to accept it — in which case this deployment MUST " +
				"run exactly one replica and its private-market metrics are not a record of anything")
		}
		logger.Warn("COMMITMENT JOURNAL IS IN-MEMORY — ALTERNATIVES_ALLOW_EPHEMERAL_JOURNAL accepted an "+
			"ephemeral cashflow series. Every capital call and distribution is DISCARDED on the next "+
			"restart, and IRR/TVPI are computed over what is left. This deployment MUST run exactly ONE "+
			"replica: two pods would fold disjoint cashflows and report different metrics for one fund",
			"fix", "set ALTERNATIVES_DATABASE_URL (or its _FILE mount); the shipped manifest runs replicas: 2",
			"gauge", "kanz_alternatives_journal_durable=0")
		journalDurable.Set(0)
		return fund.NewMemoryStore(), func() {}, nil
	}
	// MT-01d: every connection carries this deployment's tenant as the
	// `app.tenant_id` GUC, so Postgres RLS scopes all reads/writes to it. A
	// non-superuser DB role is required for FORCE RLS to apply.
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		return nil, nil, err
	}
	journalDurable.Set(1)
	return fund.NewPostgres(pool), pool.Close, nil
}

// runConsumer folds the live commitment-lifecycle FACTs into store until ctx is
// canceled. It mirrors accounting's fill-folding consumer: one bus.Consumer,
// each subject subscribed on its own goroutine (Subscribe blocks), fail-fast —
// the first non-cancellation error cancels the siblings and is returned, so a
// broken subscription brings folding down rather than running silently
// degraded (a lost capital call or NAV mark must be loud, not a quietly wrong
// IRR/TVPI). Idempotency is handled below this layer (Folder.Handle dedups on
// the event id via fund.Store.Append).
func runConsumer(ctx context.Context, cfg config.Config, store fund.Store, mesh *transport.Mesh, logger *slog.Logger, obs *observability.Provider) error {
	busMetrics := bus.NewBusMetrics(obs.Registry)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client, Metrics: busMetrics})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// WithDLQ is not optional: test/arch/bus_dlq_test.go fails the build without
	// it, and for good reason — a malformed lifecycle FACT must land somewhere
	// inspectable rather than vanish on nack.
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
	subscribe := func(subject string, handler bus.EventHandler) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("alternatives subscribing", "subject", subject, "group", cfg.ConsumerGroup)
			err := consumer.Subscribe(ctx, subject, cfg.ConsumerGroup, handler)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}()
	}
	// The journal folder, one per subject: the decoder is bound to the payload
	// type its subject carries, because nothing in the bytes says which message
	// they are. A single Folder shared across all four subjects would decode
	// every message as one type — e.g. every distribution as a capital call —
	// and silently corrupt the fund position rather than error.
	for _, subject := range cfg.Subjects {
		folder, err := consume.NewFolder(cfg.Tenant, store, consume.DecodeProto(subject))
		if err != nil {
			return err
		}
		subscribe(subject, folder.Handle)
	}
	wg.Wait()
	return firstErr
}
