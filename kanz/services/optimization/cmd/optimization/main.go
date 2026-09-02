// optimization binary entrypoint (OPT-01e). Optimizes target weights and builds
// rebalance proposals (mean-variance / risk-parity under COMP-01 mandate
// constraints), and materializes an approved proposal into OMS-01 order commands
// — the portfolio-manager's forward workflow (ROI #29). A stateless construction
// service: market inputs arrive in the request, so it boots ready; the bus
// publisher + pre-trade gate are wired behind the bridge seams at the
// composition root.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/optimization/internal/bridge"
	"github.com/eighred/kanz/services/optimization/internal/config"
	"github.com/eighred/kanz/services/optimization/internal/publish"
	"github.com/eighred/kanz/services/optimization/internal/server"
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
	// Records WHY we are stopping so the exit code can say so. Every site that
	// calls fatal.Raise below already brought the process down with stop(); the
	// only thing added is that the reason survives to the exit status (#266).
	fatal := lifecycle.NewFatal(stop)

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    "optimization",
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

	// THE AUTO-PUBLISH SWITCH (#409). Off unless asked for by name.
	//
	// With it off this service builds order commands and hands them back: the
	// optimizer proposes, a human approves, and nothing reaches the bus. With it
	// on the platform trades on its own recommendation, and that is a different
	// product — so it is stated here in as many words, at every boot, and counted.
	// A deployment must never acquire this behaviour by omission.
	autoPublished := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_optimization_auto_published_orders_total",
		Help: "Order commands published from a materialized rebalance proposal without a human " +
			"approval step. Non-zero means this deployment trades on its own recommendation.",
	})
	obs.Registry.MustRegister(autoPublished)
	factsLost := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kanz_optimization_materialization_facts_lost_total",
		Help: "ProposalMaterialized FACTs that never reached the bus. Each one is a capital action " +
			"whose record of who authorized it was lost; the orders themselves went out regardless.",
	})
	obs.Registry.MustRegister(factsLost)

	var serverOpts []server.Option
	// THE PLATFORM KILL-SWITCH, CONSTRUCTED CLOSED (#739). Built before the bus
	// so it can be handed to DialNATS as the disconnect watchdog — the gate has to
	// exist before the connection it is watching.
	//
	// Until this, an operator running kanz-halt stopped TradingView signals, the
	// gateway, the OMS and both venue adapters, and THIS service — which mints
	// order.v1.SubmitOrder commands in internal/bridge and publishes them from
	// internal/publish — carried on. It was invisible to
	// TestEveryOrderPlacingServiceHonoursTheHalt because that guard classified
	// order origins from a hand-kept list neither package was on (#738).
	haltGate := halt.NewGate(time.Now)
	producer, consumer, closeBus, err := buildBus(ctx, cfg, haltGate, logger, bus.NewBusMetrics(obs.Registry))
	if err != nil {
		logger.Error("bus init failed", "err", err)
		return 2
	}
	defer closeBus()
	// ARMED BEFORE THE ROUTES ARE MOUNTED, and the process does NOT exit if it
	// cannot be — the same posture as the api-gateway. Arm has already latched the
	// gate closed by the time it returns an error, so /v1/orders refuses to
	// publish while the optimize and propose routes, which reach no bus, keep
	// serving. Exiting here would take a stateless compute surface down over a
	// brake it only needs when it is armed.
	//
	// consumer is nil when no broker is configured, which is a deployment with no
	// publish path at all: server.WithHaltGate is then never passed either, and
	// materialize's own check refuses on a nil gate if that ever stops being true.
	if consumer != nil {
		waitHalt, herr := halt.Arm(ctx, consumer, haltGate, logger)
		if herr != nil {
			logger.Error("halt gate is NOT armed — /v1/orders will refuse to publish any rebalance "+
				"until this service is restarted", "err", herr)
		}
		if waitHalt != nil {
			go func() {
				if err := waitHalt(); err != nil && ctx.Err() == nil {
					logger.Error("halt subscription ended — rebalance publication is halted", "err", err)
				}
			}()
		}
		serverOpts = append(serverOpts, server.WithHaltGate(haltGate))

		// THE MANDATE SOURCE, WHICH IS WHAT MAKES /v1/orders REACHABLE (#751).
		//
		// Until this, /v1/propose ran Optimize then Rebalance inline — every step
		// of optimization.Propose except the mandate check — so every proposal
		// carried MandateUnchecked and bridge.ToOrders refused it. The route
		// refused out loud rather than certifying a check that never ran (#646),
		// which was right, and left the service unable to do the one thing it
		// exists for.
		//
		// THE OMS'S PATTERN, DELIBERATELY, not a second answer: the same
		// MandateRegistry fed by the same lifecycle.v1.ConfigChanged FACTs on the
		// same subject, so a mandate means here exactly what it means on the
		// capital path. A mandate this service invented, or accepted from the
		// requester, would be a control in name only.
		//
		// Broadcast, not a queue group: a mandate must reach EVERY replica, or one
		// pod proposes under governance and its neighbour proposes ungoverned.
		mandateReg := comp.NewMandateRegistry(comp.WithMandateLogger(logger))
		mandateRejected := prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kanz_compliance_mandate_rejected_total",
			Help: "Mandate messages this process consumed and could not apply. Each one leaves a " +
				"portfolio un-proposable-for until the mandate is republished.",
		})
		obs.Registry.MustRegister(mandateRejected)
		mandateConsumer := comp.NewMandateConsumer(mandateReg, logger,
			comp.WithMandateRejectionObserver(func(string, string) { mandateRejected.Inc() }))
		go func() {
			logger.Info("optimization arming the mandate registry", "subject", comp.SubjectMandateAll)
			// SubscribeBroadcastReady, not SubscribeBroadcast: Arm fires only once
			// DeliverLastPerSubject has drained, so the registry is not consulted
			// while it still holds a partial view of who is governed.
			err := consumer.SubscribeBroadcastReady(ctx, comp.SubjectMandateAll, mandateConsumer.Handle, mandateReg.Arm)
			if err != nil && ctx.Err() == nil {
				// NOT FATAL, and not silent. An unarmed registry answers "no
				// mandate governs this" for every portfolio, which produces
				// MandateUnchecked proposals — the pre-#751 behaviour, refused at
				// /v1/orders. That is the safe direction, and it must be visible
				// or it reads as "this portfolio has no constraints".
				logger.Error("mandate subscription ended — every proposal will be MandateUnchecked "+
					"and /v1/orders will refuse it until this service is restarted", "err", err)
			}
		}()

		// The classifier the SECTOR / ISSUER / ASSET_CLASS dimensions resolve
		// through. Absent, those dimensions are UNRESOLVABLE and the engine
		// REFUSES rather than passing (#640) — so a mandate naming a sector cap
		// makes the proposal infeasible with a named reason instead of silently
		// feasible, which is the correct failure and still a reason to wire it.
		refCache, rerr := cfg.RefData.NewCache(cfg.Tenant, "svc:optimization")
		if rerr != nil {
			logger.Error("optimization: the instrument classifier refused its configuration", "err", rerr)
			return 2
		}
		cfg.RefData.LogPosture(logger, "optimization")
		gate := server.MandateGate{Mandates: mandateReg, Engine: comp.NewEngine(nil)}
		if refCache != nil {
			gate.Classifier = refCache.Compliance()
		}
		serverOpts = append(serverOpts, server.WithMandateGate(gate))
	}
	// THE PROPOSAL FRESHNESS BOUND (#970), AND ITS ABSENCE IS ANNOUNCED.
	//
	// A rebalance proposal's trades are a DELTA against the holdings read at its
	// as_of, and every child this service emits is a MARKET order — so against a
	// book that has moved since, the delta is the wrong trade and nothing absorbs
	// the drift. Unset, bridge.Freshness refuses every materialization, which is
	// the fail-closed direction; a default invented here would silently restore
	// the unbounded behaviour the issue exists to remove.
	//
	// Said at ERROR when unset AND auto-publish is armed, because that is the
	// combination where a deployment believes it is trading and materializes
	// nothing — the route answers 409 to every proposal and the desk sees an
	// optimizer that "stopped working".
	if cfg.ProposalMaxAge <= 0 {
		logger.Error("no OPTIMIZATION_PROPOSAL_MAX_AGE is set — every /v1/orders request will be " +
			"REFUSED as STALE_PROPOSAL, because an unset bound is UNKNOWN rather than unlimited. " +
			"Set it to how old a proposal's inputs may be before its trade list stops describing " +
			"the current book (#970)")
	} else {
		logger.Info("proposal freshness bound armed", "max_age", cfg.ProposalMaxAge.String())
	}
	serverOpts = append(serverOpts, server.WithProposalFreshness(bridge.Freshness{MaxAge: cfg.ProposalMaxAge}))

	switch {
	case cfg.AutoPublish:
		logger.Warn("AUTO-PUBLISH IS ARMED — a materialized rebalance proposal's orders go straight to "+
			"the bus with no human approval step. Every order is still attributed to the "+
			"gateway-authenticated caller and re-checked by the OMS's pre-trade gate on admission. "+
			"A proposal is materialized only once a mandate check has passed it; with no broker "+
			"the mandate registry is unarmed, every proposal is MandateUnchecked, and /v1/orders "+
			"refuses it (#646/#751)",
			"broker", cfg.NATSURL)
		armed := publish.NewMaterializer(producer, autoPublished, factsLost)
		serverOpts = append(serverOpts, server.WithAutoPublish(
			func(tenant string) server.Materializer { return armed.ForTenant(tenant) }))
	case producer != nil:
		// A broker with the switch off is a legitimate, and the DEFAULT, posture:
		// the FACT still gets recorded, so a dry run is visible rather than
		// invisible. Say which mode this is so the two are never guessed at.
		logger.Info("auto-publish is OFF — materialized proposals are returned to the caller and " +
			"nothing is sent to the bus (set OPTIMIZATION_AUTO_PUBLISH=true to change that). Only " +
			"a proposal the mandate check passed can be materialized at all (#646/#751)")
		recorder := publish.NewRecorder(producer, factsLost)
		serverOpts = append(serverOpts, server.WithAutoPublish(
			func(tenant string) server.Materializer { return recorder.ForTenant(tenant) }))
	default:
		logger.Info("no broker configured — materialized proposals are returned to the caller and " +
			"nothing is recorded on the bus. NO MANDATE SOURCE IS REACHABLE WITHOUT A BROKER, so " +
			"every proposal is MandateUnchecked and /v1/orders refuses it (#646/#751)")
	}

	readiness := &server.Readiness{}

	// TWO LISTENERS, AND THE SPLIT IS THE POINT (#409).
	//
	// The API port carries the routes that materialize a rebalance proposal into
	// order commands, and it decides whose name goes on them from the principal
	// header the api-gateway injects. A NetworkPolicy admits only the gateway to
	// it. That policy is only worth anything while nothing ELSE forces the port
	// open — and /metrics on the same mux does exactly that, because
	// allow-observability-scrape must admit whatever port serves it.
	//
	// Every other header-trusting service on this platform shares one port and is
	// therefore reachable from kanz-observability with a self-chosen principal
	// (#232, which named the fix as a code change in each service). This is the
	// first service to make it, because a trading surface is the one place the
	// platform cannot afford to inherit that gap.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", obs.MetricsHandler())
	metricsSrv := httpserver.New(cfg.MetricsListen, metricsMux, httpserver.Standard())
	go func() {
		logger.Info("optimization metrics listening", "addr", cfg.MetricsListen)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Telemetry is not the trading path: a dead metrics listener must not
			// take the service down, but it must not be silent either — a scrape
			// target that vanishes reads as a healthy service nobody is watching.
			logger.Error("metrics server failed — this pod is now unmonitored", "err", err)
		}
	}()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutCtx)
	}()

	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger, serverOpts...), httpserver.Standard())
	go func() {
		logger.Info("optimization listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
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

	return fatal.Code()
}

// buildBus dials the spine and returns a command producer, or nil when no broker
// is configured.
//
// NO ProducerConfig.Tenant, DELIBERATELY, and for the reason the api-gateway's
// own bus states: this producer carries order COMMANDs, and a fallback tenant
// would stamp a materialized rebalance with the SERVICE's tenant when the
// caller's principal carried none — attributing a customer's capital command to
// the platform, under a value that is valid and not theirs. The tenant comes
// from the authenticated caller or the publish is refused.
//
// VerifyCommandIssuer is injected so the bus re-checks the issuer against the
// principal on ctx (AUTH-01c). This service already refuses a body-supplied
// issuer at the handler; this is the second, independent check, and it is what
// makes the refusal structural rather than a handler's good manners.
func buildBus(ctx context.Context, cfg config.Config, gate *halt.Gate, logger *slog.Logger, busMetrics *bus.BusMetrics) (*bus.Producer, *bus.Consumer, func(), error) {
	if cfg.NATSURL == "" {
		return nil, nil, func() {}, nil
	}
	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return nil, nil, func() {}, err
	}
	logger.Info("bus transport", "mtls", mesh.Enabled())
	client, err := bus.DialNATS(ctx, bus.NATSConfig{
		URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client, Metrics: busMetrics,
		// LOSING THE SPINE CLOSES THE GATE (#635), the stance the gateway, the
		// OMS and webhook-ingest all take. The halt FACT travels on this
		// connection, so a dropped one means this process can no longer establish
		// that trading is safe — and under deny-by-default, not knowing means not
		// trading. The cost is deliberate: a broker restart latches rebalance
		// publication shut until an operator resumes it. The alternative is a
		// service that reconnects, silently loses its ephemeral halt consumer,
		// and keeps publishing capital commands through a declared halt.
		OnDisconnect: func(err error) {
			gate.TripOnBusLoss(err)
			logger.Error("NATS spine lost — rebalance publication is halted, operator resume required", "err", err)
		},
		OnReconnect: func() {
			_, reason, since := gate.State()
			logger.Warn("NATS spine reconnected — gate remains latched", "reason", reason, "since", since)
		},
	})
	if err != nil {
		_ = mesh.Close()
		return nil, nil, func() {}, err
	}
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:              cfg.Source,
		ProducerVersion:     version.String(),
		VerifyCommandIssuer: auth.VerifyCommandIssuer,
	})
	if err != nil {
		_ = client.Close()
		_ = mesh.Close()
		return nil, nil, func() {}, err
	}
	// THE CONSUMER SERVES TWO SUBJECTS: the halt FACT (#739) and the mandate
	// stream (#751). The market inputs still arrive in the request body — what
	// the bus carries is the two things a caller must not be able to supply,
	// namely whether trading is permitted at all and whose rules govern the
	// portfolio.
	//
	// WithDLQ even though the broadcast path never routes there, for the reason
	// the gateway's identical consumer states: test/arch/bus_dlq_test.go exempts
	// consumers that could NEVER hold a DLQ publisher, and this one plainly can.
	// Skipping it because today's only subscription is broadcast is the loophole
	// that guard names — the exemption would survive the day a queue subscription
	// appears beside it, and the first event needing parking would be lost.
	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client))
	if err != nil {
		_ = client.Close()
		_ = mesh.Close()
		return nil, nil, func() {}, err
	}
	return producer, consumer, func() { _ = client.Close(); _ = mesh.Close() }, nil
}
