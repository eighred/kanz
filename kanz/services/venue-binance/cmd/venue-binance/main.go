// venue-binance is the Binance Spot exchange, as its own process (INFRA-M7a-2).
//
// It used to be compiled INTO the OMS behind `-tags binance`, which put the
// vendor client, HMAC request signing, and the exchange's websocket loops inside
// the address space of the process that owns order state. Now the OMS holds none
// of it: it speaks venue.v1 over mTLS and links no Binance code at all.
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
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/venueadapter/accountproof"
	"github.com/eighred/kanz/internal/venueadapter/balancerecon"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
	"github.com/eighred/kanz/internal/venueadapter/server"
	"github.com/eighred/kanz/internal/venuemargin"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/venue-binance/internal/binance"
	"github.com/eighred/kanz/services/venue-binance/internal/config"
)

// orderViewDurable reports whether this adapter's order view survives a
// restart: 1 when it is backed by Postgres, 0 when it is in-memory. An
// operator can see the degraded posture on a dashboard without reading
// logs — the healing watchdog goes blind across a restart exactly when this
// reads 0 (INFRA-M7a-2).
var orderViewDurable = prometheus.NewGauge(prometheus.GaugeOpts{
	Name:        "kanz_venue_orderview_durable",
	Help:        "1 if the venue adapter's order view is backed by Postgres (survives a restart), 0 if in-memory.",
	ConstLabels: prometheus.Labels{"venue": "binance"},
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
	if err := serve(cfg); err != nil {
		slog.Default().Error("venue-binance stopped with error", "err", err)
		return 1
	}
	return 0
}

func serve(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Records WHY we are stopping so the exit code can say so. Every site that
	// calls fatal.Raise below already brought the process down with stop(); the
	// only thing added is that the reason survives to the exit status (#266).
	fatal := lifecycle.NewFatal(stop)

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    "venue-binance",
		ServiceVersion: version.String(),
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
	if cfg.APIKey == "" || cfg.APISecret == "" {
		return errors.New("no BINANCE_API_KEY/_SECRET (or their _FILE mounts) — this adapter cannot reach the exchange")
	}

	// Probes first, so a slow exchange handshake does not look like a crash.
	readiness := &server.Readiness{}
	httpSrv := httpserver.New(cfg.HTTPListen, server.Probes(readiness, obs.MetricsHandler()), httpserver.Standard())
	go func() {
		logger.Info("venue-binance probes listening", "addr", cfg.HTTPListen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("probe server failed", "err", err)
			fatal.Raise(err)
		}
	}()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	balanceReconConfigured := balancerecon.NewGauge("binance")
	marginSourceConfigured := venuemargin.NewGauge("binance")
	obs.Registry.MustRegister(orderViewDurable, balanceReconConfigured, marginSourceConfigured)

	// The adapter's own order view — the state its workers read after the process
	// split cut them off from the OMS store.
	view, closeView, err := openView(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer closeView()

	// The async channel. No bus ⇒ no way to report a fill that arrives after
	// Execute returned, which is most fills. Refuse rather than lose them.
	if cfg.NATSURL == "" {
		return errors.New("no VENUE_BINANCE_NATS_URL — async fills (resting orders, healing FACTs) would be silently dropped")
	}
	busMetrics := bus.NewBusMetrics(obs.Registry)
	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return err
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())
	// THE PLATFORM KILL-SWITCH, CONSTRUCTED CLOSED (#635). This adapter is the LAST
	// process between a command and a live exchange, and until now it heard nothing
	// about a declared halt: kanz-halt stopped webhook signals while this kept
	// placing whatever the OMS routed. Built before the dial so it can be the
	// disconnect watchdog.
	gate := halt.NewGate(time.Now)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client,
		// LOSING THE SPINE CLOSES THE GATE. The halt FACT travels on this
		// connection; a dropped one means this process can no longer establish that
		// placing an order is safe, and the ephemeral halt consumer may not survive
		// the reconnect. Deny-by-default says that resolves to "do not place", not
		// to "carry on and hope". An operator resume is what reopens it.
		OnDisconnect: func(err error) {
			gate.TripOnBusLoss(err)
			logger.Error("NATS spine lost — this adapter will place no further orders, "+
				"operator resume required", "err", err)
		},
		OnReconnect: func() {
			_, reason, since := gate.State()
			logger.Warn("NATS spine reconnected — gate remains latched", "reason", reason, "since", since)
		},
	})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	rawProducer, err := bus.NewProducer(client, venueProducerConfig(cfg, busMetrics))
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

	// DOES EACH MAPPING SAY WHAT IT ACTUALLY TRADES? (#407)
	//
	// The symbol map is two claims side by side: a canonical id, and the symbol
	// this adapter sends to the exchange. Only the symbol decides what is bought.
	// The estate shipped "BTC-USD=BTCUSDT" — a USDT-quoted pair recorded as
	// dollars — and nothing anywhere compared the two.
	//
	// IT IS A CORRECTNESS PROBLEM, NOT A NAMING ONE. A USDT position carries USDT
	// credit exposure, and calling it USD assumes a peg this platform never states
	// and cannot monitor: on a depeg the books are wrong in a direction nobody is
	// watching, and EXPOSURE_DIMENSION_CURRENCY has already aggregated it into the
	// USD bucket where a concentration limit silently covers something else.
	//
	// Checked HERE, at the composition root, because this is the last place both
	// halves exist together: after this the symbol goes to the exchange and the id
	// goes into the ledger, and nothing downstream ever sees both.
	symbols := execution.StaticSymbolMap(parseSymbolMap(cfg.Symbols))
	if bad := symbols.Mismatches(); len(bad) > 0 {
		for _, in := range bad {
			logger.Error("SYMBOL MAP MISDESCRIBES WHAT IT TRADES — this instrument is recorded under a quote asset "+
				"the exchange does not use, so its positions, its currency exposure and any limit checked against "+
				"them are all denominated in an asset it does not hold",
				"instrument_id", in.InstrumentID, "venue_symbol", in.VenueSymbol,
				"id_claims", strings.TrimPrefix(in.InstrumentID, in.Pair.Base+"-"),
				"venue_trades", in.Pair.Quote,
				"fix", fmt.Sprintf("rename the instrument to %s-%s in BINANCE_SYMBOLS and everywhere it is referenced",
					in.Pair.Base, in.Pair.Quote))
		}
		if cfg.RequireQuoteMatch {
			return fmt.Errorf("BINANCE_SYMBOLS: %d mapping(s) name a quote asset the exchange does not "+
				"trade, and BINANCE_REQUIRE_QUOTE_MATCH=true. Rename the instruments to match what is "+
				"actually traded, or unset the requirement", len(bad))
		}
	}

	conn := binance.NewBinanceConnector(execution.VenueSettings{
		MIC:          cfg.MIC,
		Account:      cfg.Account,
		BaseURL:      cfg.BaseURL,
		APIKey:       cfg.APIKey,
		APISecret:    cfg.APISecret,
		Symbols:      symbols,
		WeightBudget: 1200,
		OnThrottle: func() {
			logger.Error("binance: REST weight budget exhausted — backing off (structural alert)")
		},
	}, cfg.WSBase)

	seam := orderview.NewSeam(view, func(err error) {
		// A blind order view makes the healing watchdog blind. Never silent.
		logger.Error("venue-binance: order view read failed — reconciliation is degraded", "err", err)
	})
	// WHAT KANZ BELIEVES THIS EXCHANGE ACCOUNT HOLDS (#418), folded from the book
	// of record's announcements (#450).
	//
	// BROADCAST, NOT A WORK QUEUE: every adapter replica needs the whole picture
	// for its own account, and a consumer group would give each replica a subset —
	// so one pod would reconcile against a balance the other did not have. A
	// balance is replicated STATE, the same argument the OMS makes for its price
	// spine and mandate registry.
	expectedBalances := balancerecon.NewView(cfg.Account,
		balancerecon.WithViewOnStale(func(age time.Duration) {
			logger.Warn("venue-binance: expected balances are too old to reconcile against — "+
				"reconciliation is SKIPPING assets rather than reporting false breaks",
				"account", cfg.Account, "age", age.String(), "subject", balancerecon.Subject)
		}),
	)
	// WithDLQ EVEN THOUGH THE BROADCAST PATH NEVER CONSULTS IT. The exemption in
	// test/arch/bus_dlq_test.go is for consumers that could NEVER hold a DLQ
	// publisher — read-only observers whose grant denies all business publish.
	// This adapter publishes fills, so it is not one of those, and skipping the
	// DLQ on the grounds that today's only subscription is broadcast is exactly
	// the loophole that guard names: a capital-path consumer would keep the
	// exemption after someone later added a queue subscription beside it.
	balanceConsumer, err := bus.NewConsumer(client, bus.WithDLQ(client))
	if err != nil {
		return err
	}
	// ARM THE BRAKE BEFORE THE gRPC SURFACE COMES UP (#635). Arm blocks until the
	// broker confirms the subscription, so a missing platform.mode.changed grant
	// surfaces here rather than as an adapter that serves Execute and honours no
	// halt.
	//
	// NOT FATAL, and not for the reason the cash spine below is not fatal. The gate
	// is already latched CLOSED by the time Arm returns an error, so this adapter
	// refuses every placement with that reason and still serves CancelOrder —
	// exactly what a halted adapter should do. Exiting instead would take the
	// cancel path down with the execute path, which is the wrong half to lose.
	waitHalt, herr := halt.Arm(ctx, balanceConsumer, gate, logger)
	if herr != nil {
		logger.Error("halt gate is NOT armed — this adapter will refuse to place any order until "+
			"it is restarted", "err", herr)
	}
	if waitHalt != nil {
		go func() {
			if err := waitHalt(); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("halt subscription ended — this adapter places no further orders", "err", err)
			}
		}()
	}
	go func() {
		logger.Info("venue-binance subscribing to the cash spine (broadcast)",
			"subject", balancerecon.Subject, "account", cfg.Account)
		if err := balanceConsumer.SubscribeBroadcast(ctx, balancerecon.Subject, expectedBalances.Handle); err != nil &&
			!errors.Is(err, context.Canceled) {
			// NOT FATAL. This adapter's job is to route and fill orders; losing the
			// balance feed degrades RECONCILIATION, and taking the pod down over a
			// reporting seam would be a self-inflicted trading outage. The view ages
			// out to unknown and reconciliation skips, loudly.
			logger.Error("venue-binance: cash-spine subscription failed — balance reconciliation is "+
				"now blind for this account", "err", err, "account", cfg.Account)
		}
	}()

	conn.Start(ctx, execution.WorkerDeps{
		Publisher: publishHealth,
		Lookup:    seam,
		Expected:  seam,
		// BOUND AT LAST (#418). accounting announces what each portfolio holds per
		// EXCHANGE ACCOUNT (#450) and this view folds the entries for the one
		// account this adapter's credential spends from. The adapter does not
		// COMPUTE a balance — the ledger is the only fold that takes trade legs,
		// cash movements and corporate actions bitemporally, and a second
		// computation here would have reconciliation comparing two of Kanz's own
		// numbers against the exchange.
		//
		// Until the first announcement arrives, and again whenever one goes stale,
		// every asset reads UNKNOWN and reconciliation SKIPS it rather than
		// comparing against zero — which would break on every asset the exchange
		// holds and teach an operator to ignore the layer.
		Balances: balancerecon.Announce(balanceReconConfigured, logger, "binance", expectedBalances),
		// NO MARGIN SOURCE ON THIS VENUE, AND THAT IS A FINDING RATHER THAN A GAP
		// (#408, control 1). This adapter is SPOT: every signed call it makes is
		// /api/v3, and GET /api/v3/account reports balances and commission rates —
		// no maintenance margin, no margin ratio, no liquidation price. There is
		// no field on that response this could honestly read.
		//
		// SO IT READS NOTHING, RATHER THAN COMPUTING SOMETHING. The tempting
		// repair is to derive a margin figure from Kanz's own positions and
		// Binance's published tier table. That is exactly what #408 rules out: a
		// reconstruction of the exchange's margin maths IS the stale book whose
		// failure mode is "the exchange sold our collateral while we were reading
		// it", and on every dashboard it would be indistinguishable from a number
		// the exchange had actually confirmed.
		//
		// Announce sets kanz_venue_margin_source_configured{venue="binance"} to 0
		// and warns at startup, so the silence on this venue's margin subject is
		// attributable rather than ambiguous. Reading Binance's margin state means
		// a margin-capable endpoint (the cross-margin or futures account API) and
		// the credentials to call it — neither exists here, and #70 holds the
		// credentials half.
		Margin: venuemargin.Announce(marginSourceConfigured, logger, "binance", nil),
		Closes: closes,
		Tenant: cfg.Tenant,
		Logger: logger,
	})

	// WHOSE MONEY DOES THIS ADAPTER SPEND? (SOV-02a)
	//
	// Ask Binance, before serving a single order. The account is the collateral
	// boundary — an exchange margins and LIQUIDATES per account — and until now it was
	// asserted by config on both sides of the wire and checked by nobody. If Binance
	// says this key belongs to a different account than we claim, we do not start:
	// every fill would be booked to the account we NAME while the exchange debits the
	// account we HOLD.
	proof, err := accountproof.Resolve(ctx, conn.Exchange(), accountproof.Want{
		Account:         cfg.Account,
		ExchangeUID:     cfg.AccountUID,
		AllowUnverified: cfg.AllowUnverifiedAccount,
	}, logger)
	if err != nil {
		return err
	}

	grpcSrv, err := newGRPCServer(ctx, cfg, conn.Venue(), view, closes, proof, gate, logger)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", cfg.GRPCListen)
	if err != nil {
		return err
	}
	go func() {
		logger.Info("venue-binance venue.v1 listening", "addr", cfg.GRPCListen, "mic", cfg.MIC, "base_url", cfg.BaseURL)
		if err := grpcSrv.Serve(lis); err != nil {
			logger.Error("grpc server failed", "err", err)
			fatal.Raise(err)
		}
	}()
	defer grpcSrv.GracefulStop()

	readiness.Set(true)
	<-ctx.Done()
	readiness.Set(false)
	// A goroutine above may have already raised a fatal (stop()'d ctx AND
	// recorded why); fatal.Err() surfaces that reason here, after every
	// shutdown step above has run, so run() can turn it into exit code 1. A
	// clean SIGTERM never raised anything, so this is nil.
	return fatal.Err()
}

// newGRPCServer serves venue.v1 over mTLS when a SPIFFE socket is configured.
//
// This endpoint submits orders to a live exchange. An unauthenticated peer that
// can reach it can trade with the fund's money, so plaintext is a dev-only
// posture and it says so at WARN — it is never silently acceptable.
func newGRPCServer(ctx context.Context, cfg config.Config, venue execution.Venue, view orderview.Store, closes execution.CloseTracker, proof execution.AccountProof, gate *halt.Gate, logger *slog.Logger) (*grpc.Server, error) {
	var opts []grpc.ServerOption
	if cfg.SPIFFESocket != "" {
		src, err := transport.NewSource(ctx, cfg.SPIFFESocket)
		if err != nil {
			return nil, err
		}
		opts = append(opts, transport.ServerOption(src, transport.AuthorizeMesh()))
		logger.Info("venue-binance: venue.v1 mTLS enabled")
	} else {
		logger.Warn("VENUE.V1 IS PLAINTEXT — no SPIFFE_ENDPOINT_SOCKET. Anyone who can reach this port can submit orders to a live exchange")
	}
	srv := grpc.NewServer(opts...)
	venuepb.RegisterVenueAdapterServiceServer(srv, server.New(venue, view, closes, proof, gate, logger))
	return srv, nil
}

// openView selects the durable order view when a DSN is set, else in-memory.
//
// By the time openView runs, config.Load has already refused to start if a
// _FILE mount was DECLARED but unreadable — that is a deployment fault, and
// it fails fast in Load, not here. So an empty cfg.DatabaseURL here can only
// mean "no durable store was ever configured" (dev/test/rig), and that is a
// real, if degraded, operating posture — not silently acceptable, exactly
// like the plaintext-gRPC posture warned about above.
func openView(ctx context.Context, cfg config.Config, logger *slog.Logger) (orderview.Store, func(), error) {
	if cfg.DatabaseURL == "" {
		logger.Warn("venue-binance: ORDER VIEW IS IN-MEMORY — no VENUE_BINANCE_DATABASE_URL (or _FILE mount). " +
			"It will be lost on restart: the healing watchdog goes blind across the restart, and in-flight " +
			"orders already live at the exchange cannot be reconciled")
		orderViewDurable.Set(0)
		return orderview.NewMemory(), func() {}, nil
	}
	pool, err := pg.NewTenantPool(ctx, cfg.DatabaseURL, cfg.Tenant)
	if err != nil {
		return nil, nil, err
	}
	orderViewDurable.Set(1)
	return orderview.NewPostgres(pool), pool.Close, nil
}

// venueProducerConfig is this adapter's producer identity. Extracted from serve
// so the one line that carries the tenant has a seam a test can reach — serve
// dials an exchange and a broker before it builds anything (#245).
//
// TENANT IS NOT REDUNDANT HERE, which is what made it easy to omit. The fill and
// StateHealed emitters stamp Event.TenantID themselves (binance_userdata.go's
// fill emitter, binance_recon.go's two StateHealed emitters), so three of this
// adapter's four publishers never
// touch this fallback. The FOURTH is the ticker feed
// (binance_connector.go), which publishes marks from a time.Ticker loop —
// no inbound delivery to inherit a tenant from, no per-event TenantID, and it
// DISCARDS its publish error. Without this field every tick was refused
// "tenant_id required" and tv-sync's MarkSource got nothing.
//
// And it did not stay quiet: the same producer is wrapped in the HealthPublisher
// that feeds readiness (see serve), which counts a failure the ticker's `_ =`
// threw away. Three instruments is one poll, DefaultPublishFailureThreshold is
// 3, so a single 5-second tick marked this adapter NOT READY — dropping it out
// of its Service and hard-erroring the OMS router on this MIC. Order execution
// was working perfectly; the mark feed took it down.
func venueProducerConfig(cfg config.Config, metrics *bus.BusMetrics) bus.ProducerConfig {
	return bus.ProducerConfig{
		Source:          cfg.Source,
		ProducerVersion: version.String(),
		Tenant:          cfg.Tenant,
		Metrics:         metrics,
	}
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
