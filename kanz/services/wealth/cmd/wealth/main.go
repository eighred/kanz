// wealth (advisory) binary entrypoint (WEALTH-01b). It aggregates a household's
// accounts into a virtual portfolio and serves the household-level exposure view
// — Aladdin Wealth atop the institutional engine (ROI #40). With
// WEALTH_DATABASE_URL set the household book is the durable Postgres store;
// without it openStore REFUSES TO START unless WEALTH_ALLOW_EPHEMERAL_BOOK=true
// says the deployment accepts serving an empty book after a restart (#261). The
// risk-engine recompute over the virtual portfolio is driven through the risk
// api/v* surface.
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
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/wealth/internal/book"
	"github.com/eighred/kanz/services/wealth/internal/config"
	"github.com/eighred/kanz/services/wealth/internal/consume"
	"github.com/eighred/kanz/services/wealth/internal/server"
)

// bookDurable reports whether the household book survives a restart: 1 when it
// is the Postgres store, 0 when it is the in-memory one an operator opted into
// with WEALTH_ALLOW_EPHEMERAL_BOOK. The gauge outlives the start-up WARN, which
// is what makes the degraded posture alertable rather than merely readable.
// Same contract as kanz_audit_log_durable (#236).
var bookDurable = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "kanz_wealth_book_durable",
	Help: "1 if the household book is backed by Postgres (survives a restart), 0 if in-memory.",
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
		ServiceName:    "wealth",
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
	obs.Registry.MustRegister(bookDurable)

	store, closeStore, err := openStore(ctx, cfg, logger)
	if err != nil {
		logger.Error("store init failed", "err", err)
		return 2
	}
	defer closeStore()

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, cfg.Tenant, store, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("wealth listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	// Household-valuation consumer (WEALTH-01b): fold live HouseholdValued FACTs
	// into the same book.Store the server reads, so the household exposure view
	// reflects live valuations. Runs concurrently with the server; both share
	// ctx so SIGTERM stops them together. Empty WEALTH_NATS_URL ⇒ read-only
	// (default), the same stance accounting/alternatives take for their
	// consumers — this is the Folder.Handle seam that had been bus-handler-
	// shaped and unreachable since it was written, because nothing ever called
	// bus.NewConsumer in this service.
	var consumers sync.WaitGroup
	if cfg.NATSURL != "" {
		consumers.Add(1)
		go func() {
			defer consumers.Done()
			if err := runConsumer(ctx, cfg, store, mesh, logger, obs); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("household consumer stopped with error", "err", err)
				fatal.Raise(err)
			}
		}()
	} else {
		logger.Info("no WEALTH_NATS_URL set — serving read endpoints only (no valuation folding)")
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

// awaitConsumers blocks until the valuation consumer has finished draining. It
// is the LAST statement in main for a reason: everything that tears this
// process down — closeStore (pool.Close), mesh.Close, obs.Shutdown — is
// deferred, so it runs after main's body and cannot be reordered ahead of this
// wait.
//
// #233: without it, main returned from <-ctx.Done(), ran a near-instant
// httpSrv.Shutdown, and fired those defers while the consumer was still
// folding. pkg/bus does not STOP a cancelled consumer, it DRAINS one —
// Subscribe deliberately spends up to DrainGrace + drainStopSlack (5s + 2s,
// pkg/bus/nats.go) after ctx.Done() handing buffered messages to the handler,
// because Stop() would abandon them and reorder the replayed log on every
// rolling deploy. Those handler calls landed on a closed pool, book.Postgres.Put
// failed, and bus.WithDLQ republished each valuation to dlq.<subject> and ACKED
// it. tools/replay refuses dlq. subjects, so the household compositions were
// gone with no supplied way back — on every deploy.
//
// THE BOUND IS A context.WithTimeout(context.Background(), N*time.Second) ON
// PURPOSE, not a bare time.After: that is the exact spelling
// test/arch/pod_disruption_and_drain_test.go's worstServiceShutdownSeconds
// greps for to derive every manifest's terminationGracePeriodSeconds from the
// source. Rewriting it as a timer would drop 10s out of the derived drain
// budget while the process still spends it, and nothing would say so.
//
// 10s against a 7s bus drain: enough headroom for the drain plus the consumer's
// own teardown, short enough that 5s (obs flush) + 15s (HTTP) + 10s here stays
// inside the 45s terminationGracePeriodSeconds every deploy manifest sets. When
// it expires we close anyway and say so loudly — a handler that ignores context
// cancellation must not hold the pod until the kubelet SIGKILLs it, which would
// abandon strictly more than this timeout does.
//
// Copied, not shared: services/risk-engine/internal/app/lifecycle.go is the
// reference implementation of this ordering and the right home for it once it
// is promoted to a package all three composition roots can import.
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
			"household store and bus client anyway; any event still in a handler will fail its " +
			"Put and be routed to dlq.<subject>")
	}
}

// openStore selects the durable Postgres household book when a DSN is set, and
// otherwise REFUSES TO START unless the deployment has said out loud that it
// accepts an ephemeral book. Returns a close func that tears down the pool (a
// no-op for the in-memory store). Both satisfy book.Store, so the server is
// identical either way.
//
// book.Postgres has existed since PARITY-02b, tested and unused: nothing ever
// constructed it, so every household this service served lived in a map and
// vanished on restart — silently, which is #261.
//
// WHY THIS ONE REFUSES, AND THE NEAR-MISS THAT MAKES IT LOOK LIKE IT SHOULD NOT.
// The WEALTH stream is deliberately shaped so an in-memory book COULD be
// rebuilt: infra/nats/bootstrap-job.yaml gives it --max-msgs-per-subject=1 and
// no max-age precisely "so DeliverLastPerSubject gives a booting consumer every
// household's latest valuation in one read". If this service armed itself that
// way, an ephemeral book would be a genuinely defensible posture and this would
// be a warn, as market-data's is.
//
// IT DOES NOT. runConsumer below uses the DURABLE QUEUE GROUP
// (bus.Consumer.Subscribe), which resumes at its last ack and never re-reads
// what it already folded — the compacted stream's one useful property is left on
// the table. So a restarted pod comes back with an EMPTY map and serves it: not
// an error, not a partial answer, an authoritative-looking exposure view of a
// household that holds nothing. That is EXEC-M13's disarmed mandate registry and
// EXEC-M20's blind compliance book, a third time. (Switching the consumer to the
// broadcast/DeliverLastPerSubject path is the real repair and is not this
// change; it alters delivery semantics for every subscriber of this subject.)
//
// Nor does a replica count rescue it: infra/deploy/wealth-deploy.yaml runs
// replicas: 2, and a queue group hands each valuation to ONE pod, so the two
// maps hold disjoint households and a query is answered by whichever pod the
// Service picked. Until the consumer arms itself from the stream, there is no
// deployment shape in which this is acceptable, so it is an opt-in rather than a
// default.
//
// config.Load has already refused to start if the _FILE mount was DECLARED but
// unreadable (secret.Read), so an empty DSN here can only mean none was ever
// configured. The shipped manifest always mounts one.
func openStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (book.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		if !cfg.AllowEphemeralBook {
			return nil, nil, errors.New("no WEALTH_DATABASE_URL (or _FILE mount): the household book would " +
				"be IN-MEMORY and every household would be DISCARDED on the next restart, rollout or " +
				"eviction. The pod would then serve an EMPTY exposure view as though it were the answer — " +
				"the consumer group resumes at its last ack and never re-reads the valuations it already " +
				"folded, so nothing refills it. Set WEALTH_DATABASE_URL, or set " +
				"WEALTH_ALLOW_EPHEMERAL_BOOK=true to accept it — in which case this deployment MUST run " +
				"exactly one replica and its exposure view is not authoritative")
		}
		logger.Warn("HOUSEHOLD BOOK IS IN-MEMORY — WEALTH_ALLOW_EPHEMERAL_BOOK accepted an ephemeral book. "+
			"Every household is DISCARDED on the next restart and the pod then serves an EMPTY exposure "+
			"view, because the durable consumer group resumes at its last ack and never re-reads. This "+
			"deployment MUST run exactly ONE replica: a queue group gives each valuation to one pod, so two "+
			"pods hold disjoint households",
			"fix", "set WEALTH_DATABASE_URL (or its _FILE mount); the shipped manifest runs replicas: 2",
			"gauge", "kanz_wealth_book_durable=0")
		bookDurable.Set(0)
		return book.NewMemoryStore(), func() {}, nil
	}
	// MT-01d: every connection carries this deployment's tenant as the
	// `app.tenant_id` GUC, so Postgres RLS scopes all reads/writes to it. A
	// non-superuser DB role is required for FORCE RLS to apply.
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		return nil, nil, err
	}
	bookDurable.Set(1)
	return book.NewPostgres(pool), pool.Close, nil
}

// runConsumer folds live wealth.v1.HouseholdValued FACTs into store until ctx
// is canceled. It mirrors accounting's fill-folding consumer: one bus.Consumer,
// each subject subscribed on its own goroutine (Subscribe blocks), fail-fast —
// the first non-cancellation error cancels the siblings and is returned, so a
// broken subscription brings folding down rather than running silently
// degraded (a stale household valuation must be loud, not a quietly wrong
// exposure view). Unlike alternatives, wealth carries one message type on one
// subject, so a single consume.NewFolder(cfg.Tenant, store, consume.DecodeProto) suffices
// — no per-subject factory is needed.
func runConsumer(ctx context.Context, cfg config.Config, store book.Store, mesh *transport.Mesh, logger *slog.Logger, obs *observability.Provider) error {
	folder, err := consume.NewFolder(cfg.Tenant, store, consume.DecodeProto)
	if err != nil {
		return err
	}

	busMetrics := bus.NewBusMetrics(obs.Registry)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// WithDLQ is not optional: test/arch/bus_dlq_test.go fails the build without
	// it, and for good reason — a malformed valuation FACT must land somewhere
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
	for _, subject := range cfg.Subjects {
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			logger.Info("wealth subscribing", "subject", subject, "group", cfg.ConsumerGroup)
			err := consumer.Subscribe(ctx, subject, cfg.ConsumerGroup, folder.Handle)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(subject)
	}
	wg.Wait()
	return firstErr
}
