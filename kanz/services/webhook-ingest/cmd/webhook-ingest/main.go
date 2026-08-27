// webhook-ingest is the entry point of the automated fund-management loop
// (Milestone 1). It securely receives TradingView Pine webhooks, records each
// as an immutable signal.v1.StrategySignal FACT, resolves the signal's size to
// an absolute quantity against live fund state, and fans it out into N
// order.v1.SubmitOrder COMMANDS on the bus — the commands the OMS already
// executes. It runs no execution logic of its own.
package main

import (
	"context"
	"errors"
	"github.com/prometheus/client_golang/prometheus"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/webhook-ingest/internal/config"
	"github.com/eighred/kanz/services/webhook-ingest/internal/ingest"
	"github.com/eighred/kanz/services/webhook-ingest/internal/server"
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Records WHY we are stopping so the exit code can say so. Every
	// fatal.Raise call below already brought the process down via stop(); the
	// only thing added is that the reason survives to the exit status (#266).
	fatal := lifecycle.NewFatal(stop)

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    "webhook-ingest",
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

	// The kill-switch, constructed CLOSED. Nothing trades until the lifecycle stream
	// tells us the system is NORMAL, and a lost spine slams it shut again. It is
	// built before the bus so it can be handed to DialNATS as the disconnect
	// watchdog — the gate has to exist before the connection it is watching.
	gate := halt.NewGate(time.Now)

	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake. This is the platform's public
	// entrance — the first hop of the trading loop — so it is also the first thing
	// that would have discovered the spine unreachable, in production.
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
	client, err := bus.DialNATS(ctx, bus.NATSConfig{
		Metrics:   busMetrics,
		URL:       cfg.NATSURL,
		Name:      cfg.Source,
		TLSConfig: mesh.Client,
		OnDisconnect: func(err error) {
			gate.TripOnBusLoss(err)
			logger.Error("NATS spine lost — trading halted, operator resume required", "err", err)
		},
		OnReconnect: func() {
			// Deliberately does NOT resume: the wire is back, the book is not
			// necessarily safe. An operator clears the latch.
			_, reason, since := gate.State()
			logger.Warn("NATS spine reconnected — gate remains latched", "reason", reason, "since", since)
		},
	})
	if err != nil {
		logger.Error("bus dial failed", "err", err)
		return 2
	}
	defer func() { _ = client.Close() }()

	// One metrics registry for both producer and consumer: NewBusMetrics registers
	// collectors, and registering the same ones twice panics.

	// No VerifyCommandIssuer: HMAC is this path's perimeter boundary, and the
	// commands issue as "strategy:{id}" (the forged-issuer guard is the gateway's
	// concern for end-user commands). The bus still enforces a non-empty issuer.
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          cfg.Source,
		ProducerVersion: version.String(),
		Metrics:         busMetrics,
	})
	if err != nil {
		logger.Error("producer init failed", "err", err)
		return 2
	}

	// The halt FACT stream is what OPENS the gate: it starts closed, and only an
	// operator ModeChanged(system → NORMAL) lets this process trade. If the
	// subscription itself dies, the gate latches shut — a process that cannot hear
	// the brake does not drive.
	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics), bus.WithDLQ(client))
	if err != nil {
		logger.Error("consumer init failed", "err", err)
		return 2
	}
	// halt.Arm, not a hand-rolled SubscribeBroadcast. The delivery policy this used
	// to spell out inline — BROADCAST, never a consumer group, because a durable
	// group meant a RESTARTED pod resumed past the operator's resume and answered
	// 423 to every signal until a human noticed — now lives in ONE place, and so
	// does the failure behaviour. That consolidation is the fix for #635: this
	// process was the only one in the estate that had it at all, so the "platform
	// kill-switch" stopped webhook signals and nothing else.
	//
	// Arm BLOCKS until the broker confirms the subscription, so a missing grant
	// surfaces here rather than as a service that starts, reports ready, and
	// honours no halt. It has already tripped the gate closed by the time it
	// returns an error; this process keeps running and refuses every signal with
	// that reason, which is what fail-closed means for an edge that is otherwise
	// useless while halted.
	waitHalt, err := halt.Arm(ctx, consumer, gate, logger)
	if err != nil {
		logger.Error("halt gate is NOT armed — this process cannot hear the platform brake and "+
			"will refuse every signal until it is restarted", "err", err)
	}
	if waitHalt != nil {
		go func() {
			if err := waitHalt(); err != nil && ctx.Err() == nil {
				logger.Error("halt subscription ended — trading halted", "err", err)
			}
		}()
	}

	// The replay defence (EXEC-M17). Cross-pod against Redis, or in-process — and
	// in-process is an EXPLICIT admission, not a default, because a per-pod nonce cache
	// silently makes every extra replica a double-trade machine.
	nonces, closeNonces, err := newNonceStore(ctx, cfg, logger)
	if err != nil {
		logger.Error("replay defence is not safe to run", "err", err)
		return 2
	}
	if closeNonces != nil {
		defer func() { _ = closeNonces.Close() }()
	}

	// THE FUND'S BOOK — the binding that was missing (EXEC-M19b).
	//
	// This wired `ingest.StaticPositions{}` — an EMPTY MAP — with the comment "M1 sim: flat
	// by default; M3+ binds the OMS projection". M3+ never bound it. So translate's CLOSE
	// path sized every flatten leg from a position of ZERO, the fan-out skipped every leg,
	// and a `close` alert produced NO ORDERS AT ALL while the webhook answered 202 Accepted.
	// A strategy that opened a position and later told Kanz to close it was silently ignored,
	// and the position stayed open. The loop could OPEN a trade and could not CLOSE one.
	//
	// The cache folds the OMS's per-venue position FACTs (EXEC-M19a) off the compacted
	// POSITION stream, so it is correct after a restart and on every replica — and it REFUSES
	// to answer until the replay has landed, because "I have not learned the book" must never
	// be rendered as "the fund is flat", which is precisely how a CLOSE gets swallowed.
	positions := ingest.NewPositionCache()

	auth := ingest.NewAuthenticator(cfg.Secrets, cfg.Allowlist, cfg.ReplayWindow, time.Now,
		ingest.WithNonceStore(nonces))
	// Alerts arriving with no `ts` (#416). These are REFUSED by default — their
	// age cannot be established — and counted here regardless of the outcome,
	// because the refusal reaches the sender in a 400 and this is what reaches
	// us. Non-zero names a strategy whose alert template needs fixing.
	unstamped := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_webhook_unstamped_signals_total",
		Help: "Alerts arriving with no source timestamp, by strategy. Their age cannot be checked, " +
			"so they are refused unless WEBHOOK_INGEST_REQUIRE_SIGNAL_TS=false. Non-zero means that " +
			"strategy's alert template is missing the `ts` field.",
	}, []string{"strategy_id"})
	obs.Registry.MustRegister(unstamped)

	// ALERTS REFUSED ON THEIR AGE (#416). The other half of the counter above,
	// and the asymmetry it removes was the odd one: an unstamped alert — which
	// configuration may ADMIT — was counted, while a stale one, which is ALWAYS
	// refused, was not. The louder failure was the invisible one.
	//
	// direction separates the two fixes: "too_old" is a delivery problem (a retry
	// storm, a partition, a paused pod), "future" is a clock problem at the
	// sender. Non-zero on either means that strategy's alerts are being dropped
	// at the perimeter and only the SENDER is being told.
	stale := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_webhook_stale_signals_total",
		Help: "Alerts refused because their source timestamp is outside " +
			"WEBHOOK_INGEST_MAX_SIGNAL_AGE, by strategy and direction " +
			"(too_old | future). Non-zero means that strategy's alerts are not being acted on.",
	}, []string{"strategy_id", "direction"})
	obs.Registry.MustRegister(stale)

	// ATTEMPTED CROSS-TENANT ORDERS (#632). Non-zero means a sender holding a
	// strategy's HMAC secret asked this platform to trade a fund that strategy is
	// not bound to — a leaked secret being pointed at another tenant's book, or a
	// bootstrap file missing a binding. Both need a human; neither is normal
	// traffic, so this is an alertable series rather than a debug counter.
	//
	// STRATEGY ONLY, NOT FUND. The strategy id is bounded by the secret table this
	// deployment holds; fund_id is an arbitrary string from the request body, and a
	// label taking it would let an unauthenticated-for-that-fund caller mint
	// unbounded time series. The fund it named is on the server's ERROR log line.
	unboundFund := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_webhook_unbound_fund_denials_total",
		Help: "Authenticated alerts refused because the strategy is not bound to the fund_id they " +
			"named, by strategy. The HMAC authenticates the strategy; the fund is a claim checked " +
			"against the bootstrap binding. Non-zero means a leaked strategy secret or a missing " +
			"binding — the fund named is in the ERROR log beside each increment.",
	}, []string{"strategy_id"})
	obs.Registry.MustRegister(unboundFund)

	pipeline, err := ingest.NewPipeline(ingest.Options{
		Auth:      auth,
		Symbols:   cfg.Symbols,
		Prices:    cfg.Prices,
		Equity:    cfg.Equity,
		Positions: positions, // the fund's REAL per-venue book (EXEC-M19b)
		Alloc:     cfg.Alloc,
		Publisher: producer,
		Gate:      gate,
		// THE TENANT COMES FROM HERE AND NOWHERE ELSE (#632). config.Load already
		// refused to return without it, so this can never be nil — and NewPipeline
		// refuses a nil one anyway, because "the field nobody assigned" is the exact
		// history being closed: the seam this replaced defaulted to the caller's own
		// fund_id and was never wired at any composition root.
		Authority:            cfg.Authority,
		MaxQuantity:          cfg.MaxQuantity,
		MaxLeverage:          cfg.MaxLeverage,
		ReplayWindow:         cfg.ReplayWindow,
		MaxSignalAge:         cfg.MaxSignalAge,
		AllowUnstampedSignal: !cfg.RequireSignalTS,
		// NAMED, NOT JUST COUNTED. A bare total would say the estate has alerts
		// whose age nothing can judge without saying which strategies to fix, and
		// WEBHOOK_INGEST_REQUIRE_SIGNAL_TS cannot be armed until that list is empty.
		OnUnstampedSignal: func(strategyID string) {
			unstamped.WithLabelValues(strategyID).Inc()
		},
		OnStaleSignal: func(strategyID string, future bool) {
			direction := "too_old"
			if future {
				direction = "future"
			}
			stale.WithLabelValues(strategyID, direction).Inc()
		},
		OnUnboundFund: func(strategyID string) {
			unboundFund.WithLabelValues(strategyID).Inc()
		},
	})
	if err != nil {
		logger.Error("pipeline init failed", "err", err)
		return 2
	}

	// Arm the book from the compacted stream, and DO NOT REPORT READY UNTIL IT HAS. A pod
	// that does not know what the fund holds must not be handed a signal that closes it: the
	// kubelet keeps it out of its Service until the replay has drained.
	go func() {
		err := consumer.SubscribeBroadcastReady(ctx, subject.VenuePositionAll, positions.Handle, positions.Arm)
		if err != nil && ctx.Err() == nil {
			logger.Error("position book subscription failed — a CLOSE signal cannot be sized without it", "err", err)
			fatal.Raise(err)
		}
	}()

	readiness := &server.Readiness{}
	// READINESS MUST REPRESENT THE REPLAY DEFENCE, not just the position book.
	//
	// Both of this pod's preconditions can fail, and until now only one of them was
	// probed. A pod whose nonce store is unreachable refuses every alert with a 503 —
	// correctly, it fails closed — while /readyz answers 200 and the pod stays in its
	// Service. That is a total ingest outage that every health signal calls healthy, on
	// the platform's public entrance. The in-process store cannot be unreachable and
	// implements nothing here, so this is a no-op on the default build.
	if probe, ok := nonces.(server.NonceStoreHealth); ok {
		readiness.TrackNonceStore(probe)
	}
	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger, pipeline, server.WithMetrics(obs.MetricsHandler()), server.WithCloudflareOnly(cfg.CloudflareOnly)), httpserver.Standard())
	go func() {
		logger.Info("webhook-ingest listening", "addr", cfg.Listen, "nats", cfg.NATSURL)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()
	// Ready = the book is learned. Until then the pod stays out of its Service, so a CLOSE
	// cannot arrive to find an empty cache and be answered with 202 and no orders.
waitForBook:
	for !positions.Armed() {
		select {
		case <-ctx.Done():
			// Shut down before the replay landed. Fall through to graceful shutdown WITHOUT
			// reporting ready: this pod never learned what the fund holds.
			break waitForBook
		case <-time.After(50 * time.Millisecond):
		}
	}
	if positions.Armed() {
		logger.Info("position book armed — the fund's holdings are known", "subject", subject.VenuePositionAll)
		readiness.Set(true)
	}

	<-ctx.Done()
	readiness.Set(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}

	// The run loop's own error and any fatal raised from a goroutine answer the
	// same question — why is this process stopping — so they meet here, after
	// every shutdown step above has run. Raise(nil) is a no-op mark.
	return fatal.Code()
}
