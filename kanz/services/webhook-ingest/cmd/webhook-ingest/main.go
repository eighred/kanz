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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"

	"github.com/eighred/kanz/internal/platform/subject"
	"github.com/eighred/kanz/internal/signal/translate"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/webhook-ingest/internal/config"
	"github.com/eighred/kanz/services/webhook-ingest/internal/ingest"
	"github.com/eighred/kanz/services/webhook-ingest/internal/server"
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
		ServiceName:    "webhook-ingest",
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

	// The kill-switch, constructed CLOSED. Nothing trades until the lifecycle stream
	// tells us the system is NORMAL, and a lost spine slams it shut again. It is
	// built before the bus so it can be handed to DialNATS as the disconnect
	// watchdog — the gate has to exist before the connection it is watching.
	gate := translate.NewGate(time.Now)

	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake. This is the platform's public
	// entrance — the first hop of the trading loop — so it is also the first thing
	// that would have discovered the spine unreachable, in production.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		logger.Error("spiffe source init failed", "err", err)
		os.Exit(2)
	}
	defer func() { _ = mesh.Close() }()
	logger.Info("bus transport", "mtls", mesh.Enabled())

	client, err := bus.DialNATS(ctx, bus.NATSConfig{
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
		os.Exit(2)
	}
	defer func() { _ = client.Close() }()

	// One metrics registry for both producer and consumer: NewBusMetrics registers
	// collectors, and registering the same ones twice panics.
	busMetrics := bus.NewBusMetrics(obs.Registry)

	// No VerifyCommandIssuer: HMAC is this path's perimeter boundary, and the
	// commands issue as "strategy:{id}" (the forged-issuer guard is the gateway's
	// concern for end-user commands). The bus still enforces a non-empty issuer.
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          cfg.Source,
		ProducerVersion: version(),
		Metrics:         busMetrics,
	})
	if err != nil {
		logger.Error("producer init failed", "err", err)
		os.Exit(2)
	}

	// The halt FACT stream is what OPENS the gate: it starts closed, and only an
	// operator ModeChanged(system → NORMAL) lets this process trade. If the
	// subscription itself dies, the gate latches shut — a process that cannot hear
	// the brake does not drive.
	consumer, err := bus.NewConsumer(client, bus.WithBusMetrics(busMetrics), bus.WithDLQ(client))
	if err != nil {
		logger.Error("consumer init failed", "err", err)
		os.Exit(2)
	}
	go func() {
		// BROADCAST, not a consumer group. The gate is constructed CLOSED and only a
		// ModeChanged FACT opens it — so a durable group meant a RESTARTED pod resumed
		// past the operator's resume and never saw it: it came back halted, reported
		// /readyz 200, and answered 423 to every signal until a human noticed. Every
		// rolling update was a silent trading outage. A broadcast subscription starts
		// at the LAST ModeChanged, so a pod that boots learns the CURRENT mode.
		// Deny-by-default survives: no FACT ever published ⇒ nothing delivered ⇒ closed.
		err := consumer.SubscribeBroadcast(ctx, translate.SubjectModeChanged, gate.Handle)
		if err != nil && ctx.Err() == nil {
			gate.Trip(lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
				"halt subscription failed: "+err.Error())
			logger.Error("halt subscription failed — trading halted", "err", err)
		}
	}()

	// The replay defence (EXEC-M17). Cross-pod against Redis, or in-process — and
	// in-process is an EXPLICIT admission, not a default, because a per-pod nonce cache
	// silently makes every extra replica a double-trade machine.
	nonces, closeNonces, err := newNonceStore(cfg, logger)
	if err != nil {
		logger.Error("replay defence is not safe to run", "err", err)
		os.Exit(2)
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
	pipeline, err := ingest.NewPipeline(ingest.Options{
		Auth:         auth,
		Symbols:      cfg.Symbols,
		Prices:       cfg.Prices,
		Equity:       cfg.Equity,
		Positions:    positions, // the fund's REAL per-venue book (EXEC-M19b)
		Alloc:        cfg.Alloc,
		Publisher:    producer,
		Gate:         gate,
		MaxSize:      cfg.MaxSize,
		MaxLeverage:  cfg.MaxLeverage,
		ReplayWindow: cfg.ReplayWindow,
	})
	if err != nil {
		logger.Error("pipeline init failed", "err", err)
		os.Exit(2)
	}

	// Arm the book from the compacted stream, and DO NOT REPORT READY UNTIL IT HAS. A pod
	// that does not know what the fund holds must not be handed a signal that closes it: the
	// kubelet keeps it out of its Service until the replay has drained.
	go func() {
		err := consumer.SubscribeBroadcastReady(ctx, subject.VenuePositionAll, positions.Handle, positions.Arm)
		if err != nil && ctx.Err() == nil {
			logger.Error("position book subscription failed — a CLOSE signal cannot be sized without it", "err", err)
			stop()
		}
	}()

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, pipeline, server.WithMetrics(obs.MetricsHandler()), server.WithCloudflareOnly(cfg.CloudflareOnly)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("webhook-ingest listening", "addr", cfg.Listen, "nats", cfg.NATSURL)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
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
