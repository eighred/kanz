// tv-sync is the reverse feedback loop of the automated fund-management system
// (Milestone 2). It folds the OMS order/fill FACT stream into a zero-truth,
// bitemporal, multi-tenant read model and serves it over the TradingView
// Broker-Integration API (REST + streaming), so Eighred traders watch the
// automated funds live on TradingView. It owns no source of truth and issues no
// orders — a pure projection.
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

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/marketdata/mark"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/tv-sync/internal/brokerapi"
	"github.com/eighred/kanz/services/tv-sync/internal/config"
	"github.com/eighred/kanz/services/tv-sync/internal/projection"
	"github.com/eighred/kanz/services/tv-sync/internal/server"
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

	if err := validateFactPriceSeparation(cfg.PriceSubjects); err != nil {
		slog.Error("subscription config refused", "err", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Records WHY we are stopping so the exit code can say so. Every
	// fatal.Raise call below already brought the process down via stop(); the
	// only thing added is that the reason survives to the exit status (#266).
	fatal := lifecycle.NewFatal(stop)

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    "tv-sync",
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

	// THE BOOK MUST SURVIVE A RESTART (EXEC-M21).
	//
	// The projection folds the fund's orders, executions and P&L into memory, and the
	// Broker API serves TradingView out of it. The bus consumer is a DURABLE GROUP: a
	// restarted pod resumes at its last ack and never re-reads what it already folded. With
	// no durable log, a pod roll left the trader looking at an EMPTY ACCOUNT — no positions,
	// no orders, zero P&L — while the fund's real positions sat open at the exchanges. And
	// nothing could rebuild it: the EXECUTION stream ages off at 24h and no table anywhere
	// persists a fill.
	//
	// The pool is tenant-scoped, as every durable service on this platform is: an unscoped
	// session cannot read or write the log at all (app_current_tenant() RAISES).
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		logger.Error("fact log unavailable — tv-sync would lose the fund's book on the next restart", "err", err)
		return 2
	}
	defer pool.Close()

	// M3.5: fold the market price spine into a live mark source so the
	// projection computes floating unrealized P&L dynamically.
	//
	// maxAge 0 — tv-sync's marks deliberately DO NOT expire, which is the
	// behaviour it has always had and is preserved here rather than inherited by
	// accident. A stale mark makes a P&L number slightly old; the projection
	// already degrades to empty when a mark is missing entirely. Whether a P&L
	// display should refuse an hours-old mark is a real question and a separate
	// decision — it is not settled by an OMS task needing a bound of its own.
	marks := mark.New(time.Now, 0)
	// WithRetention IS NOT OPTIONAL HERE, whatever its name suggests (#809). A
	// Projection built without it keeps every execution, order revision and fill
	// id for the life of the process — the leak #809 filed — and the failure is an
	// OOM kill with no other symptom. config.Load refuses a non-positive or
	// too-short TV_SYNC_RETENTION, so the value reaching this line is always a
	// window; TestTheResidentHistoryIsBounded fails the build if this argument is
	// dropped.
	proj := projection.New(time.Now, marks,
		projection.WithLog(projection.NewPostgresLog(pool), cfg.Tenant),
		projection.WithRetention(cfg.Retention))

	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		logger.Error("spiffe source init failed", "err", err)
		return 2
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())
	// ONE BusMetrics, CREATED BEFORE THE DIAL (#636). The connection's state and
	// its transitions are only exported when DialNATS is given this; a service
	// that publishes and never consumes contributes no kanz_bus_consume_total,
	// so BusConsumerStalled cannot see it and this gauge is the only signal that
	// its spine is gone.
	busMetrics := bus.NewBusMetrics(obs.Registry)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client, Metrics: busMetrics, OnDisconnect: func(error) { fatal.Raise(errors.New("tv-sync: price spine disconnected")) }})
	if err != nil {
		logger.Error("bus dial failed", "err", err)
		return 2
	}
	defer func() { _ = client.Close() }()

	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client))
	if err != nil {
		logger.Error("consumer init failed", "err", err)
		return 2
	}

	// Retain only TV's durable order FACTs. Price subscriptions keep their
	// ordinary finite tick profile on the original client.
	factTuning := bus.RetainedWorkTuning()
	factClient, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source + "-facts", TLSConfig: mesh.Client, Metrics: busMetrics, ConsumerTuning: &factTuning, OnDisconnect: func(error) { fatal.Raise(errors.New("tv-sync: fact spine disconnected")) }})
	if err != nil {
		logger.Error("fact bus dial failed", "err", err)
		return 2
	}
	defer func() { _ = factClient.Close() }()
	factConsumer, err := newFactConsumer(factClient)
	if err != nil {
		logger.Error("fact consumer init failed", "err", err)
		return 2
	}
	consumers := factConsumerRouter{facts: factConsumer, prices: consumer}

	// REBUILD THE BOOK BEFORE ANYTHING CAN READ IT OR ADD TO IT (EXEC-M21).
	//
	// This runs before the HTTP server, before the consumers, and before readiness: a pod
	// that has not replayed its fact log does not know what the fund holds, and an empty
	// account served to TradingView is not a degraded answer — it is a wrong one, and it
	// looks exactly like a fund that has never traded.
	//
	// It is also why the fold stays exactly-once across the boundary: a FACT replayed here
	// is already in the log, so if the consumer redelivers it, Append reports it stale and
	// the projection skips it rather than folding the same fill twice.
	// REGISTERED BEFORE THE REBUILD, so a pod that cannot checkpoint still exports
	// the series saying so (#809). A rule over an absent series evaluates to
	// nothing, which is the wiring defect #973, #963 and #983 each shipped once.
	registerCheckpointMetrics(obs.Registry)
	registerRetentionMetrics(obs.Registry, cfg.Retention)

	rehydrateStart := time.Now()
	if err := proj.Rehydrate(ctx); err != nil {
		logger.Error("could not rebuild the book from the fact log — refusing to serve an account we cannot vouch for", "err", err)
		return 2
	}
	rehydrateSeconds.Set(time.Since(rehydrateStart).Seconds())
	if proj.BootedFromCheckpoint() {
		rehydrateFromCheckpoint.Set(1)
	}
	logger.Info("book rebuilt from the fact log", "took", time.Since(rehydrateStart).String(),
		"tenant", cfg.Tenant, "from_checkpoint", proj.BootedFromCheckpoint())

	// THE CHECKPOINT LOOP IS WHAT MAKES THE NEXT BOOT CHEAP (#809). Without it this
	// pod rebuilds correctly and every successor pays the full replay again — a
	// degradation with no symptom other than a startup that lengthens with the
	// fund's history.
	go runCheckpoints(ctx, proj, cfg.CheckpointInterval, logger)

	// THE RETENTION LOOP IS WHAT KEEPS THE HEAP BOUNDED (#809). Rehydrate already
	// evicts as it replays, so the pod boots inside the window; without this loop
	// it would then grow out of it again for the rest of its life, which is the
	// leak with a delay on it.
	go runRetention(ctx, proj, retentionSweep, logger)

	readiness := server.NewReadiness(ctx)
	broker := brokerapi.New(proj)
	api := server.New(readiness, broker, server.WithMetrics(obs.MetricsHandler()))
	servers := newHTTPServers(cfg, api)
	listeners, err := bindHTTPListeners(ctx, servers)
	if err != nil {
		logger.Error("tv-sync bind failed", "err", err)
		return 2
	}
	for i, srv := range servers {
		go func() {
			logger.Info("tv-sync listening", "addr", srv.Addr)
			if err := srv.Serve(listeners[i]); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("http server failed", "addr", srv.Addr, "err", err)
				fatal.Raise(err)
			}
		}()
	}

	// Fold the OMS order/fill FACT stream into the projection, and the market
	// price spine into the mark source. Each subject is subscribed on its own
	// goroutine (Subscribe blocks); the first non-cancellation error cancels the
	// siblings.
	subs := map[string]bus.EventHandler{}
	foldHandler := guardedFold(readiness, cfg.Tenant, proj.Handle)
	for _, s := range projection.Subjects() {
		subs[s] = foldHandler
	}
	for _, s := range cfg.PriceSubjects {
		subs[s] = marks.Handle
	}
	go func() {
		if err := runConsumers(ctx, consumers, subs, cfg.ConsumerGroup, readiness.Set); err != nil && ctx.Err() == nil {
			logger.Error("fact consumer failed", "err", err)
			fatal.Raise(err)
		}
	}()

	<-ctx.Done()
	readiness.Set(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("http shutdown error", "addr", srv.Addr, "err", err)
			_ = srv.Close()
		}
	}

	// The run loop's own error and any fatal raised from a goroutine answer the
	// same question — why is this process stopping — so they meet here, after
	// every shutdown step above has run. Raise(nil) is a no-op mark.
	return fatal.Code()
}

type readyConsumer interface {
	SubscribeReady(context.Context, string, string, bus.EventHandler, func()) error
}

// This barrier proves initial durable catch-up only. It cannot establish facts
// that predate retained source history or qualify a fresh genesis.
func runConsumers(ctx context.Context, consumer readyConsumer, subs map[string]bus.EventHandler, group string, setReady func(bool)) error {
	if len(subs) == 0 || setReady == nil {
		return errors.New("tv-sync: readiness requires subscriptions and callback")
	}
	var readinessMu sync.Mutex
	remaining := len(subs)
	stopped := false
	setReady(false)
	defer func() { readinessMu.Lock(); stopped = true; setReady(false); readinessMu.Unlock() }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	for subject, handler := range subs {
		wg.Add(1)
		go func(subject string, handler bus.EventHandler) {
			defer wg.Done()
			var once sync.Once
			caughtUp := func() {
				once.Do(func() {
					readinessMu.Lock()
					defer readinessMu.Unlock()
					if stopped || ctx.Err() != nil {
						return
					}
					remaining--
					if remaining == 0 {
						setReady(true)
					}
				})
			}
			err := consumer.SubscribeReady(ctx, subject, group, handler, caughtUp)
			if ctx.Err() == nil {
				// Revoke before cancellation/drain: another subscription may take
				// its full drain budget while this one has already stopped.
				readinessMu.Lock()
				stopped = true
				setReady(false)
				readinessMu.Unlock()
				if err == nil {
					err = errors.New("tv-sync: durable subscription stopped unexpectedly")
				}
				select {
				case errCh <- err:
					cancel()
				default:
				}
			}
		}(subject, handler)
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		return ctx.Err()
	}
}
