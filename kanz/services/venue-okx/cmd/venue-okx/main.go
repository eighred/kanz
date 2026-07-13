// venue-okx is the OKX Spot exchange, as its own process (INFRA-M7a-3).
//
// It used to be compiled INTO the OMS behind `-tags okx`, which put the
// vendor client, HMAC request signing, and the exchange's websocket loops inside
// the address space of the process that owns order state. Now the OMS holds none
// of it: it speaks venue.v1 over mTLS and links no OKX code at all.
//
// The adapter answers on TWO channels, and both are load-bearing:
//
//   - SYNCHRONOUS (venue.v1 gRPC): Execute and CancelOrder. The OMS calls, this
//     process works the order at Binance, and returns the fills it got.
//   - ASYNCHRONOUS (the NATS bus): a resting limit order fills minutes later; the
//     user-data websocket delivers that execution report and this process
//     publishes the fill FACT. The reconciler publishes StateHealed the same way.
//     The OMS's position projector already consumes those subjects — they did not
//     change when the connector moved out.
//
// Drop the second channel and the system looks fine and loses fills. That is why
// this binary REFUSES to start its workers without a bus.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	venuepb "github.com/kanz-eng/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc"

	"github.com/kanz-eng/kanz/internal/execution"
	"github.com/kanz-eng/kanz/internal/pg"
	"github.com/kanz-eng/kanz/internal/venueadapter/orderview"
	"github.com/kanz-eng/kanz/internal/venueadapter/server"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/pkg/transport"
	"github.com/kanz-eng/kanz/services/venue-okx/internal/config"
	"github.com/kanz-eng/kanz/services/venue-okx/internal/okx"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Default().Error("config load failed", "err", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		slog.Default().Error("venue-okx stopped with error", "err", err)
		os.Exit(1)
	}
}

func run(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    "venue-okx",
		ServiceVersion: version(),
		OTLPEndpoint:   cfg.OTLPEndpoint,
		SampleRatio:    1,
	}, base)
	if err != nil {
		return err
	}
	logger := obs.Logger
	slog.SetDefault(logger)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = obs.Shutdown(shutCtx)
	}()

	// Credentials are the whole point of this process existing. Without them it
	// cannot reach the exchange, and a venue adapter that cannot trade must not
	// pretend to be ready — the OMS would route orders into it and they would die.
	if cfg.APIKey == "" || cfg.APISecret == "" || cfg.Passphrase == "" {
		return errors.New("no OKX_API_KEY/_SECRET/_PASSPHRASE (or their _FILE mounts) — this adapter cannot reach the exchange")
	}

	// Probes first, so a slow exchange handshake does not look like a crash.
	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.HTTPListen,
		Handler:           server.Probes(readiness, obs.MetricsHandler()),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("venue-okx probes listening", "addr", cfg.HTTPListen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("probe server failed", "err", err)
			stop()
		}
	}()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	// The adapter's own order view — the state its workers read after the process
	// split cut them off from the OMS store.
	view, closeView, err := openView(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeView()

	// The async channel. No bus ⇒ no way to report a fill that arrives after
	// Execute returned, which is most fills. Refuse rather than lose them.
	if cfg.NATSURL == "" {
		return errors.New("no VENUE_OKX_NATS_URL — async fills (resting orders, healing FACTs) would be silently dropped")
	}
	busMetrics := bus.NewBusMetrics(obs.Registry)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	rawProducer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          cfg.Source,
		ProducerVersion: version(),
		Metrics:         busMetrics,
	})
	if err != nil {
		return err
	}

	// EVERY async FACT this adapter emits goes through here: the fills that arrive
	// on the user-data websocket (which is MOST fills — a resting order fills long
	// after Execute returned) and the reconciler's StateHealed FACTs.
	//
	// If these stop landing, this adapter is executing orders at a live exchange
	// and losing the results. It must stop taking orders, not keep answering gRPC
	// while money moves unrecorded — so publish health feeds /readyz, the pod drops
	// out of its Service, and the OMS's router hard-errors on this MIC.
	publishHealth := bus.NewHealthPublisher(rawProducer, bus.DefaultPublishFailureThreshold)
	readiness.TrackPublisher(publishHealth)

	// The in-flight-close registry now lives HERE. The gRPC CancelOrder handler is
	// the writer; the connector's healing watchdog is the reader. Before the split
	// the OMS wrote it across an in-process pointer — it no longer tracks these at
	// all (execution.SelfHealing).
	closes := execution.NewCloseRegistry()

	conn := okx.NewOKXConnector(execution.VenueSettings{
		MIC:          cfg.MIC,
		Account:      cfg.Account,
		BaseURL:      cfg.BaseURL,
		APIKey:       cfg.APIKey,
		APISecret:    cfg.APISecret,
		Passphrase:   cfg.Passphrase,
		Symbols:      parseSymbolMap(cfg.Symbols),
		WeightBudget: 1200,
		OnThrottle: func() {
			logger.Error("okx: REST weight budget exhausted — backing off (structural alert)")
		},
	}, cfg.WSBase)

	seam := orderview.NewSeam(view, func(err error) {
		// A blind order view makes the healing watchdog blind. Never silent.
		logger.Error("venue-okx: order view read failed — reconciliation is degraded", "err", err)
	})
	conn.Start(ctx, execution.WorkerDeps{
		Publisher: publishHealth,
		Lookup:    seam,
		Expected:  seam,
		Closes:    closes,
		Tenant:    cfg.Tenant,
		Logger:    logger,
	})

	grpcSrv, err := newGRPCServer(ctx, cfg, conn.Venue(), view, closes, logger)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", cfg.GRPCListen)
	if err != nil {
		return err
	}
	go func() {
		logger.Info("venue-okx venue.v1 listening", "addr", cfg.GRPCListen, "mic", cfg.MIC, "base_url", cfg.BaseURL)
		if err := grpcSrv.Serve(lis); err != nil {
			logger.Error("grpc server failed", "err", err)
			stop()
		}
	}()
	defer grpcSrv.GracefulStop()

	readiness.Set(true)
	<-ctx.Done()
	readiness.Set(false)
	return nil
}

// newGRPCServer serves venue.v1 over mTLS when a SPIFFE socket is configured.
//
// This endpoint submits orders to a live exchange. An unauthenticated peer that
// can reach it can trade with the fund's money, so plaintext is a dev-only
// posture and it says so at WARN — it is never silently acceptable.
func newGRPCServer(ctx context.Context, cfg config.Config, venue execution.Venue, view orderview.Store, closes execution.CloseTracker, logger *slog.Logger) (*grpc.Server, error) {
	var opts []grpc.ServerOption
	if cfg.SPIFFESocket != "" {
		src, err := transport.NewSource(ctx, cfg.SPIFFESocket)
		if err != nil {
			return nil, err
		}
		opts = append(opts, transport.ServerOption(src, transport.AuthorizeMesh()))
		logger.Info("venue-okx: venue.v1 mTLS enabled")
	} else {
		logger.Warn("VENUE.V1 IS PLAINTEXT — no SPIFFE_ENDPOINT_SOCKET. Anyone who can reach this port can submit orders to a live exchange")
	}
	srv := grpc.NewServer(opts...)
	venuepb.RegisterVenueAdapterServiceServer(srv, server.New(venue, view, closes, logger))
	return srv, nil
}

// openView selects the durable order view when a DSN is set, else in-memory.
func openView(ctx context.Context, cfg config.Config) (orderview.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		return orderview.NewMemory(), func() {}, nil
	}
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		return nil, nil, err
	}
	return orderview.NewPostgres(pool), pool.Close, nil
}

// parseSymbolMap parses "BTC-USD=BTCUSDT,ETH-USD=ETHUSDT".
func parseSymbolMap(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if k, v, ok := strings.Cut(pair, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

func version() string { return "dev" }
