// oms binary entrypoint (OMS-01). Consumes the order commands
// (submit/amend/cancel), drives the order aggregate through validation → the
// pre-trade compliance gate → admission, works admitted orders via the EMS, and
// emits the lifecycle FACTs + command outcomes. A position projector folds the
// resulting fills back onto the risk engine's position-changed input. Without
// OMS_NATS_URL it serves HTTP/probes only (no command consumption).
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

	comp "github.com/kanz-eng/kanz/internal/compliance"
	"github.com/kanz-eng/kanz/internal/execution"
	"github.com/kanz-eng/kanz/internal/pg"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/services/oms/internal/compliance"
	"github.com/kanz-eng/kanz/services/oms/internal/config"
	"github.com/kanz-eng/kanz/services/oms/internal/order"
	"github.com/kanz-eng/kanz/services/oms/internal/position"
	"github.com/kanz-eng/kanz/services/oms/internal/server"
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

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("oms listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	if cfg.NATSURL != "" {
		if err := runConsumers(ctx, cfg, readiness, logger, obs); err != nil {
			logger.Error("oms consumers stopped with error", "err", err)
		}
	} else {
		readiness.Set(true)
		logger.Warn("no OMS_NATS_URL set — serving HTTP/probes only (no command consumption)")
		<-ctx.Done()
		readiness.Set(false)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
}

// runConsumers wires the bus producer + consumers and subscribes the command
// and fill subjects. Each subscription runs in its own goroutine; the first
// non-cancel error fails the group.
func runConsumers(ctx context.Context, cfg config.Config, readiness *server.Readiness, logger *slog.Logger, obs *observability.Provider) error {
	busMetrics := bus.NewBusMetrics(obs.Registry)

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          cfg.Source,
		ProducerVersion: version(),
		Metrics:         busMetrics,
	})
	if err != nil {
		return err
	}

	// OMS-01e: fill→position projector over a shared book.
	book := position.NewBook(cfg.BaseCurrency)
	projector, err := position.NewProjector(book, producer)
	if err != nil {
		return err
	}

	// OMS-01f + COMP-01c: the pre-trade gate is the COMP-01 engine resolving
	// against the mandate stream (a MandateConsumer feeds the registry from the
	// shared ConfigChanged subject) and projecting onto the live position book.
	// An empty registry resolves to "no mandate" ⇒ admit, so the OMS runs
	// correctly before any mandate is published.
	mandateReg := comp.NewMandateRegistry()
	mandateConsumer := comp.NewMandateConsumer(mandateReg, logger)
	preTrade := comp.NewPreTradeGate(comp.NewEngine(nil), compliance.NewBookSource(book), mandateReg, nil, nil, logger)
	gate := compliance.NewCOMP01Gate(preTrade, cfg.BaseCurrency)

	// OMS-01b/c: order command handler over the order store + a sim venue.
	emitter := order.NewEmitter(producer)
	// Venue set is composition-root-selected: SimVenue by default; Binance Spot +
	// its user-data/reconciliation/ticker workers under -tags binance
	// (configuredVenues is build-tag split, wired to the shared order store).
	store, closeStore, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeStore()
	venues, closeVenues := configuredVenues(ctx, cfg, store, producer, logger)
	defer closeVenues()
	router := execution.NewRouter(venues...)
	svc, err := order.NewService(store, emitter, gate, router, closeRegistry, logger)
	if err != nil {
		return err
	}

	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics))
	if err != nil {
		return err
	}

	type sub struct {
		subject string
		handler bus.EventHandler
	}
	var subs []sub
	for _, s := range cfg.CommandSubjects() {
		subs = append(subs, sub{s, svc.Handle})
	}
	for _, s := range cfg.FillSubjects() {
		subs = append(subs, sub{s, projector.Handle})
	}
	// THE PRE-TRADE GATE'S MANDATE REGISTRY — BROADCAST, NOT A WORK QUEUE.
	//
	// This fed from a durable CONSUMER GROUP, which resumes at its last ack. So a
	// RESTARTED OMS came back with an EMPTY registry and its pre-trade compliance
	// gate PASSED EVERY ORDER: the control did not fail, it DISARMED — silently, on
	// every rolling update, with the pod reporting ready throughout (EXEC-M13).
	//
	// A broadcast subscription over the per-portfolio mandate subjects replays
	// DeliverLastPerSubject, so this process boots holding the mandate IN FORCE for
	// every portfolio. It is also a broadcast because a mandate must reach EVERY
	// replica — a group would arm one OMS pod and leave the other ungoverned.
	mandateSub := comp.SubjectMandateAll

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for _, s := range subs {
		wg.Add(1)
		go func(s sub) {
			defer wg.Done()
			logger.Info("oms subscribing", "subject", s.subject, "group", cfg.ConsumerGroup)
			err := consumer.Subscribe(ctx, s.subject, cfg.ConsumerGroup, s.handler)
			if err != nil && !errors.Is(err, context.Canceled) {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(s)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("oms arming the pre-trade mandate registry", "subject", mandateSub)
		err := consumer.SubscribeBroadcast(ctx, mandateSub, mandateConsumer.Handle)
		if err != nil && !errors.Is(err, context.Canceled) {
			once.Do(func() {
				firstErr = err
				cancel()
			})
		}
	}()
	readiness.Set(true)
	wg.Wait()
	readiness.Set(false)
	return firstErr
}

// openStore selects the durable Postgres order store when a DSN is set
// (EXEC-M7c), otherwise the in-memory store. Returns a close func that tears
// down the pool (a no-op for the in-memory store). Both satisfy order.Store, so
// the command handler and the venue adapters share one store either way.
//
// The choice is a SAFETY choice, not a persistence one. order.Store.Create is
// the admission gate that decides which delivery of an order routes to a live
// venue; MemoryStore enforces it with a mutex, so its guarantee stops at the
// process boundary. Two replicas over two maps both admit the same order_id and
// the fund trades twice. Postgres enforces it with a PRIMARY KEY, which holds
// across replicas — so a multi-replica OMS requires OMS_DATABASE_URL.
func openStore(ctx context.Context, cfg config.Config) (order.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		return order.NewMemoryStore(), func() {}, nil
	}
	// MT-01d: every connection carries this deployment's tenant as the
	// `app.tenant_id` GUC, so Postgres RLS scopes all order reads/writes to it
	// (the authenticated-session-GUC pattern). A non-superuser DB role is
	// required for FORCE RLS to apply.
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		return nil, nil, err
	}
	return order.NewPostgres(pool), pool.Close, nil
}

// version is the service version stamped on telemetry. Hardcoded until the build
// injects a git SHA / semver (CICD-01a/c ldflags).
func version() string { return "dev" }
