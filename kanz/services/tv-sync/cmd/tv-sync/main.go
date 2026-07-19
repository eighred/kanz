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
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/kanz-eng/kanz/internal/marketdata/mark"
	"github.com/kanz-eng/kanz/internal/pg"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/pkg/transport"
	"github.com/kanz-eng/kanz/services/tv-sync/internal/brokerapi"
	"github.com/kanz-eng/kanz/services/tv-sync/internal/config"
	"github.com/kanz-eng/kanz/services/tv-sync/internal/projection"
	"github.com/kanz-eng/kanz/services/tv-sync/internal/server"
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
		ServiceName:    "tv-sync",
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
		os.Exit(2)
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
	proj := projection.New(time.Now, marks, projection.WithLog(projection.NewPostgresLog(pool), cfg.Tenant))

	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		logger.Error("spiffe source init failed", "err", err)
		os.Exit(2)
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client})
	if err != nil {
		logger.Error("bus dial failed", "err", err)
		os.Exit(2)
	}
	defer func() { _ = client.Close() }()

	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client))
	if err != nil {
		logger.Error("consumer init failed", "err", err)
		os.Exit(2)
	}

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
	rehydrateStart := time.Now()
	if err := proj.Rehydrate(ctx); err != nil {
		logger.Error("could not rebuild the book from the fact log — refusing to serve an account we cannot vouch for", "err", err)
		os.Exit(2)
	}
	logger.Info("book rebuilt from the fact log", "took", time.Since(rehydrateStart).String(), "tenant", cfg.Tenant)

	readiness := &server.Readiness{}
	broker := brokerapi.New(proj)
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, broker, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("tv-sync listening", "addr", cfg.Listen, "nats", cfg.NATSURL)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	// Fold the OMS order/fill FACT stream into the projection, and the market
	// price spine into the mark source. Each subject is subscribed on its own
	// goroutine (Subscribe blocks); the first non-cancellation error cancels the
	// siblings.
	subs := map[string]bus.EventHandler{}
	for _, s := range projection.Subjects() {
		subs[s] = proj.Handle
	}
	subs[cfg.PriceSubject] = marks.Handle
	go func() {
		if err := runConsumers(ctx, consumer, subs, cfg.ConsumerGroup); err != nil && ctx.Err() == nil {
			logger.Error("fact consumer failed", "err", err)
			stop()
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
}

func runConsumers(ctx context.Context, consumer *bus.Consumer, subs map[string]bus.EventHandler, group string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	for subject, handler := range subs {
		wg.Add(1)
		go func(subject string, handler bus.EventHandler) {
			defer wg.Done()
			if err := consumer.Subscribe(ctx, subject, group, handler); err != nil && ctx.Err() == nil {
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

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "dev"
}
