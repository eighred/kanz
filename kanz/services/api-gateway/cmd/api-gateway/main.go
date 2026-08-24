// api-gateway is the external BFF (API-01c/d): an authenticated,
// rate-limited, versioned REST surface over the risk-engine's query.v1 gRPC
// API. It dials the risk-engine over mTLS (SEC-01b), transcodes JSON↔proto,
// and applies the edge middleware chain (auth → rate-limit → idempotency →
// signing → version) before forwarding. The only governed entry point for
// external clients and UIs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/prometheus/client_golang/prometheus"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	operatorpb "github.com/eighred/kanz/kanz-schemas-go/operator/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/revocation"
	"github.com/eighred/kanz/internal/tenantgen"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/authbus"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/config"
	"github.com/eighred/kanz/services/api-gateway/internal/control"
	"github.com/eighred/kanz/services/api-gateway/internal/gateway"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
	"github.com/eighred/kanz/services/api-gateway/internal/orders"
	"github.com/eighred/kanz/services/api-gateway/internal/proxy"
)

// THE SERVER'S OWN BACKSTOPS, FOR THE TWO LEAKS A REQUEST DEADLINE CANNOT REACH.
//
// Every handler budget on this gateway bounds an UPSTREAM call. Neither covers the
// connection itself, and until these two fields were set this server had no bound on it
// at all: ReadHeaderTimeout was the only one, and it stops timing the moment the last
// header byte arrives.
//
//   - WriteTimeout is the slow-CLIENT case. A caller that reads the response one byte a
//     minute — or a wedged intermediary that stops reading entirely — pins a goroutine and
//     a file descriptor for as long as it likes. With the gateway as the sole ingress for
//     orders, enough of those and no order can be submitted at all.
//   - IdleTimeout is the keep-alive case, and its ABSENCE is the trap: with both it and
//     ReadTimeout at zero, net/http applies NO idle bound, so every keep-alive connection
//     ever opened is held until the peer closes it. That is not a slow leak under a
//     pooling proxy; it is the steady state.
//
// WHERE THESE SIT IN THE NESTING. WriteTimeout is a bound on EVERY route this server
// serves, so it has to clear the LONGEST handler budget among them or it silently becomes
// the real limit — and the way it fires is the worst of all of them: a severed connection,
// no status, no body, nothing logged upstream. The longest are control's
// testConnectionTimeout (90s) and proxy.copilotForwardTimeout (90s), so:
//
//	nginx proxy-read-timeout (120s) > gatewayWriteTimeout (105s) > handler budgets (≤90s)
//
// Above 90s so the handler's own 502/504 and its log line are what the caller gets; below
// the edge's 120s so the gateway is the layer that gives up first and can say so, rather
// than waiting to be cut by nginx. test/arch/probe_deadline_nesting_test.go holds both
// halves — including that removing either field fails, since absence is the bug.
const gatewayWriteTimeout = 105 * time.Second

// gatewayIdleTimeout must EXCEED the keep-alive idle timeout of whatever pools connections
// in front of this server, not merely be small. ingress-nginx defaults
// upstream-keepalive-timeout to 60s; set this below that and the gateway closes idle
// connections nginx still believes it may reuse, and the race shows up as intermittent
// 502s under no load at all — a harder fault to read than the leak this closes. 120s is
// twice that default.
const gatewayIdleTimeout = 120 * time.Second

// THE POOLING SIDE OF THE SAME ORDERING, ON THE PROXY'S OUTBOUND CONNECTIONS.
//
// gatewayIdleTimeout decides when THIS server retires a connection nginx pooled to
// it. proxyIdleConnTimeout is the mirror image one hop in: when the gateway retires
// a connection IT pooled to wealth/datamaster/tv-sync/copilot. It must sit BELOW
// those services' own IdleTimeout (httpserver.Standard().Idle, 120s), so the
// gateway is the side that gives the connection up. Get it the wrong way round and
// the upstream closes a socket the gateway is about to send on; net/http retries an
// idempotent request over a fresh connection, so it does not usually surface as an
// error — it surfaces as latency nobody can attribute, and on a non-idempotent
// request as a 502 under no load at all.
//
// Left at zero — which is what a bare &http.Transport{} means, and what the mTLS
// branch used to build — an idle connection is NEVER retired, so this ordering has
// no chance to hold.
const proxyIdleConnTimeout = 90 * time.Second

// http.DefaultTransport allows 2 idle connections PER HOST. This gateway fans out
// to four upstreams and is the only ingress for the estate, so on any real read
// burst the third concurrent request to a service paid for a fresh TCP handshake
// and, on the mesh, a fresh mTLS handshake — a cost chosen by a standard-library
// default rather than by anyone here.
const proxyMaxIdleConnsPerHost = 32

// Idempotency window. The TTL bounds how long a repeated Idempotency-Key is
// recognised; the max bounds the per-pod replay cache only, since the claim that
// carries correctness may be Redis-backed.
const (
	idempotencyTTL = time.Minute
	idempotencyMax = 10_000
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

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName: cfg.Source, ServiceVersion: version.String(), OTLPEndpoint: cfg.OTLPEndpoint, SampleRatio: 1,
	}, base)
	if err != nil {
		slog.Default().Error("observability init failed", "err", err)
		return 2
	}
	logger := obs.Logger
	slog.SetDefault(logger)
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = obs.Shutdown(sctx)
	}()

	if cfg.RiskEngineAddr == "" {
		logger.Error("API_GATEWAY_RISK_ENGINE_ADDR is required")
		return 2
	}
	conn, err := dialRiskEngine(ctx, cfg, logger)
	if err != nil {
		logger.Error("risk-engine dial failed", "err", err)
		return 2
	}
	defer func() { _ = conn.Close() }()

	// The OMS's order-history surface (#399). Absent unless configured, and a
	// FATAL rather than a degraded start when it is configured and cannot be
	// dialled — the same stance the control plane takes, for the same reason: a
	// history route that half-exists reports "no orders" for a config error, and
	// "this portfolio has never traded" is the most misleading answer available.
	var (
		ordersRead      orderpb.OrderQueryServiceClient
		instrumentsRead venuepb.VenueQueryServiceClient
	)
	if cfg.OMSReadAddr != "" {
		// SAME TRANSPORT RULE AS THE RISK ENGINE, not a plaintext shortcut: this
		// upstream carries a portfolio's trading history, which is at least as
		// identifying as its risk. dialUpstream is the risk-engine dialler's own
		// body, extracted so the two cannot drift into different postures.
		omsConn, oerr := dialUpstream(ctx, cfg, cfg.OMSReadAddr, logger)
		if oerr != nil {
			logger.Error("api-gateway: OMS read surface configured but unusable", "err", oerr)
			return 2
		}
		defer func() { _ = omsConn.Close() }()
		ordersRead = orderpb.NewOrderQueryServiceClient(omsConn)
		// ONE CONNECTION, TWO CONTRACTS (#406). The tradeable-pair catalogue is
		// served by the same OMS on the same port, so it needs no second address
		// to be configured, and there is no deployment in which one of the two is
		// reachable and the other is not.
		instrumentsRead = venuepb.NewVenueQueryServiceClient(omsConn)
		logger.Info("api-gateway: order history and instrument catalogue fronted", "addr", cfg.OMSReadAddr)
	} else {
		logger.Info("api-gateway: no API_GATEWAY_OMS_READ_ADDR — /v1/portfolios/{id}/orders and /v1/instruments not registered")
	}

	handler := gateway.New(querypb.NewRiskQueryServiceClient(conn), ordersRead, instrumentsRead, cfg.ApproveRole, logger)

	// PER-TENANT RISK ENGINES (#668). risk-engine holds its positions per
	// deployment (pg.NewTenantPool with cfg.Tenant, #97), so a tenant with its
	// own rendered engine has its own book — and answering it from the platform
	// engine would return ANOTHER book's exposure and VaR as its own, with a 200.
	// A risk number is what a limit is checked against, so this is the worst
	// class of wrong answer available on this surface.
	//
	// BOUNDED AT STARTUP, one connection per configured tenant, all closed by the
	// defers below. Not created on first sight of a tenant: that would be an
	// unbounded map keyed by a string arriving on a request, held for the process
	// lifetime, in the one process every request passes through.
	//
	// grpc.NewClient is LAZY, so this costs no sockets — a tenant that never
	// queries never dials, and a tenant whose engine is down does not affect
	// anyone else's startup.
	if len(cfg.PerTenantUpstreams) > 0 {
		perTenantRisk := make(map[string]querypb.RiskQueryServiceClient, len(cfg.PerTenantUpstreams))
		for _, tenant := range cfg.PerTenantUpstreams {
			addr, aerr := tenantRiskEngineAddr(cfg.RiskEngineAddr, tenant)
			if aerr != nil {
				logger.Error("api-gateway: cannot derive a per-tenant risk-engine address",
					"tenant", tenant, "base", cfg.RiskEngineAddr, "err", aerr)
				return 2
			}
			tconn, terr := dialUpstream(ctx, cfg, addr, logger)
			if terr != nil {
				// FATAL, matching the platform engine and the OMS read surface
				// above: a risk route that half-exists answers a configuration
				// error with a number, and "this portfolio has no exposure" is the
				// most misleading answer this surface can give.
				logger.Error("api-gateway: per-tenant risk-engine unusable", "tenant", tenant, "addr", addr, "err", terr)
				return 2
			}
			defer func() { _ = tconn.Close() }()
			perTenantRisk[tenant] = querypb.NewRiskQueryServiceClient(tconn)
			logger.Info("api-gateway: per-tenant risk engine fronted", "tenant", tenant, "addr", addr)
		}
		if handler, err = handler.WithPerTenantRisk(perTenantRisk); err != nil {
			logger.Error("api-gateway: per-tenant risk clients refused", "err", err)
			return 2
		}
	}

	// Order write surface (OMS-01d): publish order commands to the spine, with
	// the AUTH-01c forged-issuer guard on the producer. Nil publisher ⇒ the
	// write routes 503 (read-only gateway).
	// THE PLATFORM KILL-SWITCH, CONSTRUCTED CLOSED (#635). Built before the bus so
	// it can be handed to DialNATS as the disconnect watchdog — the gate has to
	// exist before the connection it is watching.
	//
	// Until this, an operator running kanz-halt stopped TradingView signals and
	// this route — the one authz.go calls the path that "reaches a live exchange"
	// — kept publishing order COMMANDs.
	gate := halt.NewGate(time.Now)
	producer, consumer, closeBus, err := buildBus(ctx, cfg, gate, logger, bus.NewBusMetrics(obs.Registry))
	if err != nil {
		logger.Error("order write surface init failed", "err", err)
		return 2
	}
	defer closeBus()
	// ARMED BEFORE THE ROUTES ARE MOUNTED, and the process does NOT exit if it
	// cannot be. Arm has already latched the gate closed by the time it returns an
	// error, so POST /v1/orders answers 423 with that reason while the read routes
	// and — deliberately — POST /v1/orders/{id}/cancel keep serving. Exiting here
	// would take the exits down with the entrance.
	//
	// consumer is nil on a read-only gateway (no API_GATEWAY_NATS_URL), which has
	// no write surface to brake; orders.Handler answers 503 there and never
	// consults the gate.
	if consumer != nil {
		waitHalt, herr := halt.Arm(ctx, consumer, gate, logger)
		if herr != nil {
			logger.Error("halt gate is NOT armed — POST /v1/orders will refuse every order until "+
				"this gateway is restarted", "err", herr)
		}
		if waitHalt != nil {
			go func() {
				if err := waitHalt(); err != nil && ctx.Err() == nil {
					logger.Error("halt subscription ended — the order write surface is halted", "err", err)
				}
			}()
		}
	}
	ordersHandler := orders.New(producer, cfg.ApproveRole, gate)

	// AUTH-01d: every capability decision this gateway makes is recorded.
	//
	// THE GATEWAY IS THE PLATFORM'S SOLE IDENTITY AUTHORITY AND RECORDED ITS
	// DECISIONS NOWHERE — not to the observation stream, not even to a log. Every
	// other authorization surface in the estate had at least an slog recorder.
	//
	// It shares the ORDER PRODUCER deliberately: one connection, one broker
	// identity (SEC-M3 authenticates per connection). A DecisionLog is an
	// OBSERVATION, and the producer's forged-issuer guard runs on COMMANDs only,
	// so the two event kinds do not interfere.
	recorder, closeRecorder := buildDecisionRecorder(producer, obs.Registry, logger)
	defer closeRecorder()

	// Phase-7 read surfaces (SVCWIRE-01b/c): wealth/datamaster/copilot routes
	// behind the same edge chain, forwarded over the SEC-01b mTLS mesh. With no
	// upstream addresses configured the backend is nil and the routes 503.
	proxyHandler, err := buildProxy(ctx, cfg, logger)
	if err != nil {
		return 2
	}

	// The control plane (OPS-M2b). Absent unless an address is configured, and a
	// FATAL rather than a degraded start when it is configured and cannot be reached
	// securely — an operator surface that half-exists is worse than one that does
	// not, because the TUI would report "no control plane" for a config error.
	var ctlHandler *control.Handler
	if cfg.OperatorAddr != "" {
		opConn, err := dialOperator(ctx, cfg, logger)
		if err != nil {
			logger.Error("api-gateway: control plane configured but unusable", "err", err)
			return 2
		}
		defer func() { _ = opConn.Close() }()
		ctlHandler = control.New(operatorpb.NewOperatorServiceClient(opConn), logger)
		logger.Info("api-gateway: control plane fronted", "addr", cfg.OperatorAddr, "role", cfg.OperatorRole)
	} else {
		logger.Info("api-gateway: no API_GATEWAY_OPERATOR_ADDR — /v1/control routes not registered")
	}

	authn, oidcAuth, revocations, err := buildAuthenticator(cfg, obs, logger)
	if err != nil {
		return 2
	}

	var ready atomic.Bool
	// THE IDEMPOTENCY CLAIM, AND WHICH ONE IS IN FORCE (#721).
	//
	// The claim is what makes a repeated Idempotency-Key at-most-once. Redis-backed
	// it spans replicas; in memory it does not, and this deployment runs replicas: 2
	// — so the two are NOT interchangeable and must not look the same in a log.
	idemClaims, idemCloser, err := newIdempotencyClaims(cfg, idempotencyTTL, idempotencyMax, logger)
	if err != nil {
		logger.Error("api-gateway: idempotency claims refused their configuration", "err", err)
		return 2
	}
	if idemCloser != nil {
		defer func() { _ = idemCloser.Close() }()
	}
	if idemClaims != nil {
		logger.Info("idempotency claims are CROSS-POD (redis) — a client retry is recognised on "+
			"either replica", "ttl", idempotencyTTL.String())
	} else {
		logger.Warn("idempotency claims are PER-POD (in-memory) — with replicas > 1 a client retry "+
			"that lands on the other replica is NOT recognised as a retry and WILL be executed "+
			"again; on /v1/orders that is a second live order from one client intent. Set "+
			"API_GATEWAY_REDIS_URL on a binary built with -tags redis",
			"ttl", idempotencyTTL.String(), "max", idempotencyMax)
	}

	router, err := buildRouter(cfg, handler, ordersHandler, proxyHandler, ctlHandler, obs, &ready, logger, recorder, authn, idemClaims)
	if err != nil {
		return 2
	}
	// The only server in the estate that overrides the standard bounds, because it
	// is the only one with an ingress in front of it and per-route budgets of its
	// own. The two it inherits are deliberate: ReadHeader is the estate slowloris
	// bound, and Read bounds a slow SENDER only — it does not reach a running
	// handler, so it is not a hidden cap on the 90s /v1/ask budget.
	std := httpserver.Standard()
	srv := httpserver.New(cfg.Listen, router, httpserver.Timeouts{
		ReadHeader: std.ReadHeader,
		Read:       std.Read,
		Write:      gatewayWriteTimeout,
		Idle:       gatewayIdleTimeout,
	})

	// The serve loop is the only thing that can die AFTER startup has completed
	// (rule 6, #266): lifecycle.Fatal lets the goroutine that observes the
	// failure hand it to run() without an os.Exit of its own, and stop()
	// cancelling ctx is what makes <-ctx.Done() below the join that guarantees
	// the write happens-before the read.
	fatal := lifecycle.NewFatal(stop)

	// READINESS MEANS "THIS GATEWAY CAN AUTHENTICATE SOMEBODY", NOT "THE PORT IS
	// OPEN" (#457).
	//
	// It used to mean the latter: ready.Store(true) sat unconditionally at the top
	// of this goroutine, so a gateway whose issuer did not exist bound its port,
	// logged success, answered /healthz and /readyz with ok — and 401'd every
	// request. Kubernetes routed traffic to it because the probe said to. The
	// estate shipped exactly that, pointed at a hostname that has never resolved,
	// and the only symptom was universal rejection with three green signals above
	// it.
	//
	// primeIssuer blocks until the issuer answers, so the pod stays OUT of the
	// Service until it can verify a token. It never returns an error: an
	// unreachable issuer is retried rather than fatal, because crash-looping the
	// sole identity authority over a dependency blip turns a recoverable outage
	// into a total one, and would make the deploy ORDER of two services a hard
	// constraint. On a genuinely wrong configuration this still does the right
	// thing — the rollout never completes, so the pods that CAN authenticate keep
	// serving instead of being replaced by pods that cannot.
	//
	// THE REVOCATION FEED IS PRIMED FIRST, AND FOR A NARROWER REASON THAN IT
	// LOOKS (#532). The safety property does not live here: revocation.Cache
	// refuses every request until it has fetched the feed once, so a pod that
	// somehow reported itself ready with an empty cache would answer 503, not
	// admit a revoked caller. What this ordering buys is that such a pod stays
	// OUT of the Service instead of joining it and 503ing real traffic — the
	// same argument primeIssuer makes one line down, for the same reason.
	go func() {
		if primeRevocations(ctx, revocations, obs.Registry, issuerRetryInterval, logger) {
			// The refresh loop only starts once a first fetch has landed. Started
			// before it, a failing loop would log the same error the prime already
			// logs, twice, and tell an operator nothing new.
			go revocations.Run(ctx, func(err error) {
				// ERROR, not WARN. Every failure here ages the cached feed toward
				// its ceiling, past which this gateway refuses everybody. An
				// operator has one warning before that, and this is it.
				logger.Error("api-gateway: revocation feed refresh failed — the cached feed is "+
					"ageing toward its ceiling, past which every request is refused",
					"err", err, "max_age", revocations.MaxAge())
			})
		}
		primeIssuer(ctx, oidcAuth, obs.Registry, cfg.OIDCIssuer, issuerRetryInterval, &ready, logger)
	}()

	go func() {
		if oidcAuth == nil {
			// The dev HS256 arm validates with a secret already in hand — there is
			// no provider to reach, so there is nothing to wait for.
			ready.Store(true)
		}
		logger.Info("api-gateway listening", "addr", cfg.Listen, "upstream", cfg.RiskEngineAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			fatal.Raise(err)
		}
	}()

	<-ctx.Done()
	ready.Store(false)
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}

	return fatal.Code()
}

// buildBus dials the spine and returns the producer the gateway publishes
// through, or nil when no NATS URL is configured (the read-only gateway: order
// routes 503 and decisions fall back to the log). The returned func closes the
// client on shutdown.
//
// IT RETURNS THE PRODUCER RATHER THAN THE ORDER HANDLER (#352) because two
// things now publish: the OMS-01d order write surface and the AUTH-01d decision
// recorder. Dialling a second connection for the recorder would give one process
// two client identities on a broker that authenticates per connection (SEC-M3).
func buildBus(ctx context.Context, cfg config.Config, gate *halt.Gate, logger *slog.Logger, busMetrics *bus.BusMetrics) (*bus.Producer, *bus.Consumer, func(), error) {
	if cfg.NATSURL == "" {
		logger.Warn("api-gateway: order write surface disabled (no API_GATEWAY_NATS_URL)")
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
		Metrics: busMetrics,
		URL:     cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client,
		// LOSING THE SPINE CLOSES THE GATE (#635), the same stance webhook-ingest
		// has always taken. The halt FACT travels on this connection, so a dropped
		// one means this process can no longer establish that trading is safe —
		// and under deny-by-default, not knowing means not trading. The cost is
		// real and deliberate: a broker restart latches the order write surface
		// shut until an operator resumes it. The alternative is a gateway that
		// reconnects, silently loses its ephemeral halt consumer, and keeps
		// accepting orders through a declared halt.
		OnDisconnect: func(err error) {
			gate.TripOnBusLoss(err)
			logger.Error("NATS spine lost — the order write surface is halted, operator resume required", "err", err)
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
	// NO ProducerConfig.Tenant, DELIBERATELY. This producer carries order
	// COMMANDs, and a fallback tenant here would stamp a submit whose token
	// carried no tenant claim with the gateway's own tenant — attributing a
	// customer's capital command to the platform, under a value that is valid and
	// not theirs. The order path must REFUSE such a token instead. The decision
	// recorder, which has no delivery to inherit a tenant from either, gets its
	// fallback scoped to itself via authbus.WithFallbackTenant.
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
	// THE CONSUMER EXISTS FOR ONE SUBJECT: the halt FACT (#635).
	//
	// WithDLQ EVEN THOUGH THE BROADCAST PATH NEVER CONSULTS IT, for the reason
	// venue-binance's identical consumer states: the exemption in
	// test/arch/bus_dlq_test.go is for consumers that could NEVER hold a DLQ
	// publisher, and this gateway plainly can. Skipping it on the grounds that
	// today's only subscription is broadcast is exactly the loophole that guard
	// names — the exemption would survive the day someone adds a queue
	// subscription beside it, and the first event that needed parking would be
	// lost instead. It needs no dlq.* grant: SubscribeBroadcast never routes
	// there, so nothing is ever published.
	consumer, err := bus.NewConsumer(client, bus.WithDLQ(client))
	if err != nil {
		_ = client.Close()
		_ = mesh.Close()
		return nil, nil, func() {}, err
	}
	logger.Info("api-gateway: order write surface enabled")
	return producer, consumer, func() { _ = client.Close(); _ = mesh.Close() }, nil
}

// authDecisionsLost counts AUTH-01d decisions that never reached the observation
// stream, by reason. A counter and not only a log line: a decision recorded to
// stdout is gone at the next rollout, so logging a DROPPED one to the same stdout
// reproduces the defect one layer down. Registered only when there is a bus —
// exported at zero with no broker it would read as "nothing was lost" when in
// truth nothing was ever published.
var authDecisionsLost = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "kanz_api_gateway_auth_decisions_lost_total",
	Help: "AUTH-01d authorization decisions that could not be published to the observation stream.",
}, []string{"reason"})

// buildDecisionRecorder returns the AUTH-01d recorder for the capability mux.
//
// With a producer it publishes to platform.authz.decision. WITHOUT one it falls
// back to slog rather than to nothing: the read-only gateway still makes
// authorization decisions, and "no bus" must not silently mean "no audit trail".
// Neither arm is a no-op, which is the point — this service recorded nowhere
// before #352.
func buildDecisionRecorder(producer *bus.Producer, reg prometheus.Registerer, logger *slog.Logger) (auth.DecisionRecorder, func()) {
	if producer == nil {
		logger.Warn("api-gateway: no API_GATEWAY_NATS_URL — AUTH-01d authorization decisions are " +
			"recorded to the LOG ONLY, so they do not survive a restart and cannot be queried " +
			"beside the FACTs they justified")
		return auth.NewSlogRecorder(logger), func() {}
	}
	reg.MustRegister(authDecisionsLost)
	rec, err := authbus.NewBusRecorder(producer,
		// The FALLBACK only: each decision is stamped with the deciding
		// principal's own tenant when it has one. This covers the decision made
		// when there is NO principal — an unauthenticated caller, or a token
		// with no tenant claim — which is a platform-level event and belongs
		// under the platform's own tenant.
		authbus.WithFallbackTenant(bus.SystemTenant),
		authbus.WithErrorHandler(func(err error) {
			authDecisionsLost.WithLabelValues("publish_error").Inc()
			logger.Error("an AUTH-01d decision could not be published — it exists only in this log line",
				"err", err)
		}),
		authbus.WithOverflowHandler(func(*observationpb.DecisionLog) {
			authDecisionsLost.WithLabelValues("queue_full").Inc()
			logger.Error("an AUTH-01d decision was DROPPED because the recorder queue is full — " +
				"the observation stream is now missing decisions this gateway did make")
		}),
	)
	if err != nil {
		// Unreachable: NewBusRecorder errors only on a nil producer, excluded above.
		// Degrade rather than refuse to start — an unrecorded gateway is bad, a gateway
		// that will not start is an outage.
		logger.Error("decision recorder init failed — falling back to the log", "err", err)
		return auth.NewSlogRecorder(logger), func() {}
	}
	logger.Info("api-gateway: authorization decisions publish to the observation stream")
	return rec, rec.Close
}

// buildProxy wires the Phase-7 read surfaces (SVCWIRE-01c). It collects the
// configured upstream base URLs and builds a mesh backend over an mTLS HTTP
// client (SEC-01b) when a SPIFFE socket is set, plaintext for local/dev. With no
// upstreams configured it returns a nil-backed handler whose routes 503 — the
// same disabled-surface shape as the order write surface.
func buildProxy(ctx context.Context, cfg config.Config, logger *slog.Logger) (*proxy.Handler, error) {
	bases := map[proxy.Service]string{}
	if cfg.WealthAddr != "" {
		bases[proxy.ServiceWealth] = cfg.WealthAddr
	}
	if cfg.DataMasterAddr != "" {
		bases[proxy.ServiceDataMaster] = cfg.DataMasterAddr
	}
	if cfg.CopilotAddr != "" {
		bases[proxy.ServiceCopilot] = cfg.CopilotAddr
	}
	if cfg.TVSyncAddr != "" {
		bases[proxy.ServiceTVSync] = cfg.TVSyncAddr
	}
	if cfg.OptimizationAddr != "" {
		bases[proxy.ServiceOptimization] = cfg.OptimizationAddr
	}
	if cfg.AccountingAddr != "" {
		bases[proxy.ServiceAccounting] = cfg.AccountingAddr
	}
	if cfg.ComplianceAddr != "" {
		bases[proxy.ServiceCompliance] = cfg.ComplianceAddr
	}
	if cfg.AuditAddr != "" {
		bases[proxy.ServiceAudit] = cfg.AuditAddr
	}
	// The funding surface is registered by the ROLE, not by the address (#535). It
	// is logged either way: an absent route must be a stated posture, not something
	// an operator discovers from a 404.
	if cfg.FundRole == "" {
		logger.Warn("api-gateway: no API_GATEWAY_FUND_ROLE — POST /v1/portfolios/{id}/cash-movements " +
			"is NOT registered, so this gateway answers 404 to it. That is deliberate: authz.Fund " +
			"would be carried by no role, and a registered route nobody can reach answers 403 to " +
			"everyone while looking like a working control (#535)")
	} else {
		logger.Info("api-gateway: funding surface fronted", "role", cfg.FundRole)
	}
	// THE OVERRIDE SURFACE, ON THE SAME STANCE AND FOR A SHARPER REASON (#539).
	//
	// datamaster's maker-checker workflow was reachable by nobody until #539: it
	// registers propose, approve and pending-overrides, and this gateway — its only
	// permitted caller under network-policies.yaml — routed none of them.
	//
	// THE DANGEROUS CONFIGURATION IS DATAMASTER ARMED AND THIS UNSET. With
	// DATAMASTER_REQUIRE_DUAL_CONTROL=true every override is held for a second
	// signature that no client can give, so exception resolution stops and the book
	// goes on being valued off prices the system already flagged. That flag lives in
	// another service's environment and this process cannot read it, so config.Load
	// cannot refuse the combination — which is exactly why the absence is logged
	// here rather than left to a 404 somebody meets later.
	if cfg.ApproveRole == "" {
		if cfg.DataMasterAddr != "" {
			logger.Warn("api-gateway: datamaster is wired but no API_GATEWAY_APPROVE_ROLE — the " +
				"override propose/approve/pending routes are NOT registered, so this gateway answers " +
				"404 to them and NOBODY can resolve a pricing exception through it. If datamaster runs " +
				"with DATAMASTER_REQUIRE_DUAL_CONTROL=true, every override is held for a signature no " +
				"client can give (#539)")
		} else {
			logger.Info("api-gateway: no override surface (no datamaster upstream and no approver)")
		}
	} else if cfg.DataMasterAddr == "" {
		// AN APPROVER NAMED OVER NO BACKEND IS THE INVERSE FAILURE, AND IT USED TO
		// READ AS SUCCESS. This branch logged "override surface fronted" for any
		// non-empty role, including a deployment with no datamaster at all — where
		// the override routes do not register, because they are conditional on the
		// client as well as the role. "Nothing configured" and "checked, and fine"
		// printed the same line.
		//
		// Found by arming API_GATEWAY_APPROVE_ROLE in dev/docker-compose.yml to make
		// #539's path exercisable and discovering the rig fronts neither dual-control
		// backend, so the setting was inert and this log said otherwise.
		logger.Warn("api-gateway: API_GATEWAY_APPROVE_ROLE names " + cfg.ApproveRole +
			" but no API_GATEWAY_DATAMASTER_ADDR — the override propose/approve/pending routes are " +
			"NOT registered. Granting that role to a person now gives them a capability no route " +
			"honours, which is the #535 shape inverted: the control looks armed from the config and " +
			"answers 404 from the estate (#539)")
	} else {
		logger.Info("api-gateway: override surface fronted", "role", cfg.ApproveRole)
	}

	// AND THE SAME QUESTION FOR ACT THREE, WHICH HAS A SECOND PRECONDITION. The
	// order approve route rides the bus and registers on the role alone, but the
	// queue that tells an approver WHAT to sign is a read off the OMS. Without it
	// an approver can sign and cannot discover: the approve route refuses an empty
	// digest with 400, a held order is deliberately absent from the orders table,
	// and the digest otherwise rides OrderPendingApproval on NATS.
	if cfg.ApproveRole != "" && cfg.OMSReadAddr == "" {
		logger.Warn("api-gateway: API_GATEWAY_APPROVE_ROLE is set but no API_GATEWAY_OMS_READ_ADDR — " +
			"GET /v1/orders/pending-approvals is NOT registered. An approver can sign an order id they " +
			"already know and has no way to learn one, so a held order is discoverable only by reading " +
			"NATS (#539)")
	}

	// AND ACT TWO (#562). The posture is logged on BOTH sides for the reason the
	// override branch above records: "nothing configured" and "checked, and fine"
	// printed the same line here once already, and the inverse — a role named over
	// no backend — read as success.
	//
	// config.validateAuth already refuses the dangerous direction (an address with
	// no signatory), so what is left to say is the two harmless-but-silent ones.
	switch {
	case cfg.MandateRole == "" && cfg.ComplianceAddr == "":
		logger.Info("api-gateway: no mandate-change surface (no compliance upstream and no signatory) — " +
			"cmd/kanz-mandate remains the only way to change a mandate in this deployment, and it " +
			"takes two INVOCATIONS rather than two people (#562)")
	case cfg.MandateRole == "":
		// Unreachable via config.Load, which refuses this pairing. Kept because a
		// Config built in a test or by a future caller can still reach it, and a
		// silent 404 here is what #535 cost.
		logger.Warn("api-gateway: compliance is wired but no API_GATEWAY_MANDATE_ROLE — the mandate " +
			"propose/approve/pending routes are NOT registered, so this gateway answers 404 to them " +
			"and NOBODY can change a mandate through it (#562)")
	case cfg.ComplianceAddr == "":
		logger.Warn("api-gateway: API_GATEWAY_MANDATE_ROLE names " + cfg.MandateRole +
			" but no API_GATEWAY_COMPLIANCE_ADDR — the mandate routes ARE registered and every one " +
			"of them 503s. Granting that role to a person now gives them an authority no upstream " +
			"honours (#562)")
	default:
		logger.Info("api-gateway: mandate-change surface fronted", "role", cfg.MandateRole)
	}

	// THE COMPLIANCE READ SURFACE (#627), logged on both sides for the same reason.
	//
	// "Fronted" here means SIX ROUTES ARE MOUNTED AND AN UPSTREAM ANSWERS THEM,
	// which is the claim #539 found this gateway making over a config with zero
	// routes mounted. Both halves are checked before it is printed.
	switch {
	case cfg.AuditRole == "" && cfg.AuditAddr == "":
		logger.Info("api-gateway: no compliance read surface (no audit upstream and no reader) — " +
			"the tenant's audit history, decision lineage and SOC2 evidence are not readable " +
			"through this gateway by anyone (#627)")
	case cfg.AuditRole == "":
		// Unreachable via config.Load, which refuses this pairing, and kept for the
		// same reason the mandate branch above is.
		logger.Warn("api-gateway: audit is wired but no API_GATEWAY_AUDIT_ROLE — the six compliance " +
			"read routes are NOT registered, so this gateway answers 404 to them and NOBODY can " +
			"read the audit trail through it (#627)")
	case cfg.AuditAddr == "":
		logger.Warn("api-gateway: API_GATEWAY_AUDIT_ROLE names " + cfg.AuditRole +
			" but no API_GATEWAY_AUDIT_ADDR — the audit routes ARE registered and every one of " +
			"them 503s. Granting that role to an auditor now gives them an authority no upstream " +
			"honours (#627)")
	default:
		logger.Info("api-gateway: compliance read surface fronted", "role", cfg.AuditRole,
			"upstream", cfg.AuditAddr)
	}

	if len(bases) == 0 {
		logger.Warn("api-gateway: Phase-7 read surfaces disabled (no upstream addresses)")
		return proxy.New(nil, proxyRoles(cfg)), nil
	}

	// ONE CLIENT, BUILT THE SAME WAY ON BOTH BRANCHES; ONLY THE TLS DIFFERS.
	//
	// The plaintext branch used to be `client := http.DefaultClient`, and that was
	// wrong for two reasons that outlived the timeout it was filed for (#235).
	// http.DefaultClient is a SHARED MUTABLE GLOBAL: any package linked into this
	// binary that sets DefaultClient.Timeout or swaps DefaultTransport silently
	// retunes every proxied read, from somewhere no reader of this file would look.
	// And it carries http.DefaultTransport, whose MaxIdleConnsPerHost is 2 — for a
	// gateway fanning out to four upstreams that is connection churn on the hot
	// read path, chosen by nobody.
	//
	// NO Timeout FIELD ON EITHER BRANCH, DELIBERATELY — it used to carry 30s and that
	// was a bug of the exact kind the TUI already paid for. http.Client.Timeout is
	// enforced independently of the request context, so it was a SECOND bound on the
	// same call and min(context, 30s) won invisibly: it capped /v1/ask at 30s
	// underneath the copilot's own five-minute completion budget, and it applied only
	// on the mTLS path, so dev and production were bounded differently for reasons
	// nothing stated. The one bound lives on the call (proxy.forwardBudget), where the
	// route that needs a different budget can say so and a test can read it.
	// test/arch/probe_deadline_nesting_test.go fails the build if it comes back.
	tr := &http.Transport{
		MaxIdleConnsPerHost: proxyMaxIdleConnsPerHost,
		IdleConnTimeout:     proxyIdleConnTimeout,
	}
	if cfg.SPIFFESocket != "" {
		src, err := transport.NewSource(ctx, cfg.SPIFFESocket)
		if err != nil {
			logger.Error("api-gateway: proxy SPIFFE source failed", "err", err)
			return nil, err
		}
		tr.TLSClientConfig = transport.ClientTLSConfig(src, transport.AuthorizeMesh())
		logger.Info("api-gateway: Phase-7 upstreams mTLS enabled", "services", len(bases))
	} else {
		// NOT A REFUSAL TO START, AND THAT IS A CHOICE — see #98.
		//
		// "Fail loudly" is about a misconfiguration that LOOKS HEALTHY. This one does
		// not: it warns on every boot, and since the per-call budgets landed both
		// branches carry identical bounds, so the asymmetry that made the plaintext
		// path the dangerous one ("dev and production bounded differently, and dev
		// could hang forever") no longer exists. What remains is a SECURITY posture
		// difference — no mesh authentication — and that is #98's question, decided
		// for the estate rather than re-decided here. Refusing here would also take
		// out the only environment this gateway can currently be run in end to end:
		// the dev kind rig has no SPIRE.
		logger.Warn("api-gateway: Phase-7 upstreams plaintext (no API_GATEWAY_SPIFFE_SOCKET)")
	}
	backend, err := proxy.NewMeshBackend(bases, &http.Client{Transport: tr}).
		WithPerTenantUpstreams(cfg.PerTenantUpstreams)
	if err != nil {
		// A tenant id this gateway cannot route to is a typo in an env var, and
		// the only symptom downstream would be a 503 against a Service that was
		// never rendered. Refuse at boot, where the message can name the value.
		return nil, err
	}
	return proxy.New(backend, proxyRoles(cfg)), nil
}

// proxyRoles is the ONE place the optional-capability roles are handed to the
// proxy handler.
//
// IT IS A FUNCTION BECAUSE THERE ARE TWO RETURN PATHS ABOVE, and the second one
// is the disabled-backend branch nothing exercises in a normal deployment. Built
// twice by hand, a third role added to one literal and not the other would leave
// the nil-backend gateway registering a different route set from the live one —
// discoverable only by running a gateway with no upstreams and noticing a 404
// where a 503 belonged.
func proxyRoles(cfg config.Config) proxy.Roles {
	return proxy.Roles{Fund: cfg.FundRole, Approve: cfg.ApproveRole, Mandate: cfg.MandateRole, Audit: cfg.AuditRole}
}

// issuerProbeTimeout bounds one attempt to reach the issuer. Generous, because
// the cost of a slow answer is a slower rollout while the cost of giving up too
// early is a pod flapping out of the Service under load.
const issuerProbeTimeout = 15 * time.Second

// issuerRetryInterval is how often an unreachable issuer is retried. It is the
// gateway's own floor and deliberately independent of the JWKS rate limiter,
// which governs the REQUEST path: this loop runs while the pod is receiving no
// traffic at all, so nothing else is generating attempts to be limited against.
//
// A CONST, AND IT MUST STAY ONE. It was briefly a var so the readiness tests
// could shrink it — with a comment arguing that was harmless because nothing in
// the running service writes it. That was wrong, and `-race` in CI proved it
// within one merge: the tests' cleanup wrote this variable while a probe
// goroutine was still reading it, which is a data race in a package the whole
// estate authenticates through. The interval is now a PARAMETER, so a test
// chooses its own without there being any shared mutable state to choose it in.
const issuerRetryInterval = 10 * time.Second

// primeIssuer blocks until the OIDC issuer answers with a usable key set, then
// marks the gateway ready (#457). It returns immediately on the HS256 arm.
//
// WHAT "USABLE" MEANS IS THE POINT. This is not a reachability ping: Prime runs
// the real discovery, checks that the document's issuer matches the configured
// one, and requires a JWKS with at least one key. A provider answering for a
// DIFFERENT issuer — the case where tokens would be validated against keys
// belonging to somebody else — fails here rather than passing a liveness check.
//
// THE FAILURE LOG NAMES THE CONSEQUENCE, NOT JUST THE ERROR. An operator reading
// "connection refused" goes looking at the network. An operator reading "this
// gateway can authenticate NOBODY until this resolves" goes looking at the
// issuer, which is where the fault is.
func primeIssuer(ctx context.Context, a *auth.OIDCAuthenticator, reg prometheus.Registerer, issuer string, retryEvery time.Duration, ready *atomic.Bool, logger *slog.Logger) {
	if a == nil {
		return
	}
	// Registered only on the OIDC arm, and set to 0 BEFORE the first attempt. A
	// gauge that appeared only on success would be absent exactly when it matters,
	// and absent reads as "no data" rather than as "this gateway cannot
	// authenticate anybody" — the distinction this whole change exists to make.
	reachable := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kanz_api_gateway_oidc_issuer_reachable",
		Help: "1 when the OIDC issuer's key set has been fetched and this gateway can verify tokens; " +
			"0 when it never has, in which case every request is rejected regardless of the token.",
	})
	reg.MustRegister(reachable)
	reachable.Set(0)

	for attempt := 1; ; attempt++ {
		pctx, cancel := context.WithTimeout(ctx, issuerProbeTimeout)
		err := a.Prime(pctx)
		cancel()
		if err == nil {
			reachable.Set(1)
			ready.Store(true)
			logger.Info("api-gateway: OIDC authentication enabled — issuer reached and keys held",
				"issuer", issuer, "attempts", attempt)
			return
		}
		// ERROR, not WARN. The gateway is the platform's sole identity authority
		// and it currently authenticates nobody; that is an outage, and logging it
		// at a level operators filter out is how it stayed invisible.
		logger.Error("api-gateway: CANNOT REACH THE OIDC ISSUER — this gateway can verify no token "+
			"from anybody, and stays OUT of the Service (/readyz 503) until it can. Every request "+
			"would be rejected regardless of how valid its token is",
			"issuer", issuer, "err", err, "attempt", attempt, "retry_in", retryEvery)

		select {
		case <-ctx.Done():
			return
		case <-time.After(retryEvery):
		}
	}
}

// buildAuthenticator selects the token validator. It returns the concrete OIDC
// authenticator alongside the interface, because the composition root has to do
// something with it that the request path cannot: PROVE IT CAN REACH ITS ISSUER
// before the gateway declares itself ready (#457). Nil on the HS256 arm, which
// has no provider to reach.
//
// One of these two arms always runs: config.Load refuses to return a Config with
// neither an OIDC issuer nor a JWT secret, so the gateway cannot reach here
// unauthenticated. There is no third arm, and middleware.Auth refuses every
// request if a nil Authenticator ever reaches it anyway.
func buildAuthenticator(cfg config.Config, obs *observability.Provider, logger *slog.Logger) (middleware.Authenticator, *auth.OIDCAuthenticator, *revocation.Cache, error) {
	switch {
	case cfg.OIDCIssuer != "":
		oidc, err := auth.NewOIDCAuthenticator(auth.OIDCConfig{
			Issuer:      cfg.OIDCIssuer,
			Audience:    cfg.OIDCAudience,
			JWKSURI:     cfg.OIDCJWKSURI,
			TenantClaim: cfg.OIDCTenantClaim,
			RolesClaim:  cfg.OIDCRolesClaim,
		})
		if err != nil {
			logger.Error("api-gateway: OIDC config invalid", "err", err)
			return nil, nil, nil, err
		}
		// The degraded-key posture, exported for as long as it lasts (#242).
		// Registered here rather than inside NewGatewayMetrics because it only
		// exists on this arm: the HS256 validator has no provider to lose.
		middleware.RegisterOIDCKeyGauge(obs.Registry, oidc.KeysUnrevalidated)
		// DELIBERATELY NOT LOGGED AS "ENABLED" HERE. Constructing this proves only
		// that the issuer string is non-empty; it performs no network I/O. The line
		// that used to sit here — "OIDC authentication enabled", naming the issuer —
		// was printed by a gateway pointed at a hostname that has never resolved,
		// and was one of three signals telling an operator everything was fine while
		// nothing could be authenticated (#457). The success line is now printed by
		// primeIssuer, after keys are actually in hand.

		// PER-SUBJECT REVOCATION (#532). config.Load has already refused an OIDC
		// arm with no feed URL, so this cannot be reached unconfigured.
		revs, err := revocation.New(revocation.Config{URL: cfg.RevocationsURI})
		if err != nil {
			logger.Error("api-gateway: revocation feed config invalid", "err", err)
			return nil, nil, nil, err
		}
		// WRAPPED, NOT CONSULTED SOMEWHERE ELSE. The revocation check has to sit
		// between "this token verifies" and "this caller is admitted", and the
		// only way to make that true of EVERY authenticator this gateway will
		// ever build is to make the wrap the thing that PRODUCES one. #535 and
		// #539 were both the other shape: a control that existed, was wired, and
		// was in nobody's path.
		revoking, err := middleware.NewRevoking(oidcAuthenticator{oidc}, revs)
		if err != nil {
			logger.Error("api-gateway: revocation wiring invalid", "err", err)
			return nil, nil, nil, err
		}
		return revoking, oidc, revs, nil
	default:
		// Reachable only with API_GATEWAY_ALLOW_DEV_HS256=true — config.Load
		// refuses the arm otherwise, so this WARN now describes a deliberate
		// choice rather than an omission nobody noticed (#242).
		//
		// "NO REVOCATION PATH" IS NOW LITERAL AND NOT RHETORICAL (#532). The OIDC
		// arm above is wrapped in middleware.Revoking against identity's feed;
		// this arm is not, because a dev rig runs no identity service to serve
		// one. That asymmetry is deliberate and it is the reason this arm must
		// stay unreachable without API_GATEWAY_ALLOW_DEV_HS256.
		logger.Warn("api-gateway: using dev HS256 validator — a shared symmetric secret with no " +
			"revocation path, and with NO per-subject revocation either: disabling an account does " +
			"not stop the token it already holds (API_GATEWAY_ALLOW_DEV_HS256=true). Set " +
			"API_GATEWAY_OIDC_ISSUER for production")
		return middleware.NewJWTAuthenticator(cfg.JWTSecret), nil, nil, nil
	}
}

// revocationsProbeTimeout bounds one feed fetch during priming. Same figure as
// issuerProbeTimeout and for the same reason: a probe that can hang forever is a
// pod that never reports why it is not ready.
const revocationsProbeTimeout = issuerProbeTimeout

// primeRevocations blocks until identity's revocation feed has been fetched
// once, then returns true. It returns false immediately on the HS256 arm (no
// cache) and on context cancellation.
//
// WHY THE POD WAITS FOR THIS AT ALL. revocation.Cache refuses every request
// until its first successful fetch — that is the cold-start clause of #532, and
// it is enforced in the cache rather than here, so it holds no matter what this
// composition root does. What waiting buys is that a pod which would refuse
// everybody does not join the Service and start refusing real traffic. A gateway
// that cannot read the feed is not a gateway that should be taking orders.
//
// IT RETRIES RATHER THAN FAILING FATAL, exactly like primeIssuer: crash-looping
// the sole ingress over a dependency blip turns a recoverable outage into a
// total one, and would make the deploy ORDER of two services a hard constraint.
func primeRevocations(ctx context.Context, c *revocation.Cache, reg prometheus.Registerer, retryEvery time.Duration, logger *slog.Logger) bool {
	if c == nil {
		return false
	}
	// REGISTERED BEFORE THE FIRST ATTEMPT, so the series exists while the answer
	// is "never fetched". A gauge that appeared only on success would be absent
	// exactly when it matters, and absent reads as "no data" rather than as "this
	// gateway is refusing everybody".
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_api_gateway_revocations_usable",
		Help: "1 when this gateway holds a revocation feed within its ceiling and is therefore " +
			"enforcing per-subject revocation; 0 when it is not, in which case every request is " +
			"refused 503 regardless of the token.",
	}, func() float64 {
		if c.Usable() {
			return 1
		}
		return 0
	}))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kanz_api_gateway_revocations_age_seconds",
		Help: "Seconds since the newest successful fetch of identity's revocation feed, or -1 if " +
			"there has never been one. Climbing toward the ceiling is the only warning before " +
			"this gateway starts refusing every request.",
	}, func() float64 {
		age, ok := c.Age()
		if !ok {
			// -1, NOT 0. Zero is what a perfectly fresh feed reports, and "never
			// fetched" is the opposite of that.
			return -1
		}
		return age.Seconds()
	}))

	for attempt := 1; ; attempt++ {
		pctx, cancel := context.WithTimeout(ctx, revocationsProbeTimeout)
		err := c.Refresh(pctx)
		cancel()
		if err == nil {
			logger.Info("api-gateway: per-subject revocation enabled — identity's feed is in hand",
				"attempts", attempt, "max_age", c.MaxAge())
			return true
		}
		// ERROR, not WARN. Until this succeeds the gateway refuses every request,
		// which is an outage — a deliberate, fail-closed one, but an outage, and
		// logging it at a level operators filter out is how it would stay invisible.
		logger.Error("api-gateway: CANNOT READ IDENTITY'S REVOCATION FEED — this gateway cannot "+
			"tell a disabled account from an active one, so it refuses every request (503) and "+
			"stays OUT of the Service until it can",
			"err", err, "attempt", attempt, "retry_in", retryEvery)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(retryEvery):
		}
	}
}

// buildRouter wires the public probes/metrics/openapi (un-gated) and the /v1
// risk + order + Phase-7 read routes behind the edge middleware chain.
func buildRouter(cfg config.Config, h *gateway.Handler, o *orders.Handler, p *proxy.Handler, ctl *control.Handler, obs *observability.Provider, ready *atomic.Bool, logger *slog.Logger, recorder auth.DecisionRecorder, authn middleware.Authenticator, idemClaims bus.Deduper) (http.Handler, error) {
	// EVERY /v1 ROUTE DECLARES WHAT IT TAKES TO REACH IT (SEC-M2).
	//
	// The gateway used to wrap all of /v1 in ONE role check, so `GET /v1/portfolios/{id}/
	// exposure` and `POST /v1/orders` were guarded identically: the token handed to an
	// analyst to look at exposure would submit an order to a live exchange.
	//
	// authz.Mux takes the capability as a required parameter of registration, so a route
	// cannot be added without deciding who may call it — the code would not compile. The
	// grants below are the only place a role becomes an authority.
	grants := authz.Grants{
		// The baseline role admits a caller to the gateway at all (SEC-M1) and lets them
		// READ. It must never carry Trade: every authenticated caller holds it.
		cfg.RequiredRole: {authz.Read},
		// The trade role MOVES CAPITAL. A trader can obviously also read — a control that
		// made traders carry two tokens would be routed around within a week.
		cfg.TradeRole: {authz.Read, authz.Trade},
		// The operator role RUNS THE ESTATE (OPS-M2b): provisioning and draining nodes,
		// and writing the exchange credentials the venue adapters sign with. It carries
		// Read for the same reason the trade role does — an operator who cannot see the
		// system they are operating will be handed a second token within a week.
		//
		// It does NOT carry Trade, and Trade does not carry Operate. config.validateAuth
		// refuses to start if this role collides with either of the others, because a
		// collision here silently merges two authorities that exist to be separate.
		cfg.OperatorRole: {authz.Read, authz.Operate},
	}
	// THE FUND ROLE MOVES THE FUND'S OWN CAPITAL (#535, #415): subscriptions,
	// redemptions and fees posted to the book of record. authz.Fund was declared,
	// demanded by POST /v1/portfolios/{id}/cash-movements, and carried by NO ROLE
	// IN THIS MAP — three roles for four capabilities — so that route answered 403
	// to every principal that exists while reading as a working control.
	//
	// ADDED CONDITIONALLY RATHER THAN AS A FOURTH LINE ABOVE, because Grants is
	// keyed by the role STRING and cfg.FundRole is legitimately empty (the route is
	// then not registered, and the deployment answers 404). Writing `cfg.FundRole:
	// {...}` unconditionally would put a "" key in the policy, and any token whose
	// roles claim carried an empty entry would match it — the dev HS256 arm takes
	// that claim verbatim. Not reachable today, and not worth leaving in a map that
	// decides who may move the fund's cash.
	//
	// It carries Read for the same reason the two above do, and Read is not a
	// widening: every caller who reaches /v1 at all already holds the baseline role.
	if cfg.FundRole != "" {
		grants[cfg.FundRole] = []authz.Capability{authz.Read, authz.Fund}
	}
	// THE APPROVER GIVES THE SECOND SIGNATURE (#539, #410 act one) on an act
	// somebody else proposed. Conditional for the same reason the fund role is —
	// Grants is keyed by the role STRING, and an unconditional entry would put a ""
	// key in the map that decides who may clear an override.
	//
	// IT CARRIES Read, AND THAT IS NOT A WIDENING BUT IT IS THE ONE WORTH SAYING
	// OUT LOUD. An approver who cannot read the exception, the instrument and the
	// price it is being overridden to is signing something unread, which is the
	// whole failure the second signature exists to prevent. Read is what makes the
	// eyes in "four eyes" real.
	//
	// IT DOES NOT CARRY Trade, Fund OR Operate, and validateAuth refuses to start
	// if this role name collides with any of them. A signatory who also holds the
	// authority to act alone is not a control — and it fails silently, because
	// internal/dualcontrol only checks that the approver is a different SUBJECT.
	if cfg.ApproveRole != "" {
		grants[cfg.ApproveRole] = []authz.Capability{authz.Read, authz.Approve}
	}
	// THE MANDATE SIGNATORY CHANGES THE CONSTRAINT ITSELF (#562, #410 act two):
	// what a portfolio may hold, in what concentration, at what leverage. Both
	// halves of that act — proposing and signing — are this one capability,
	// because four-eyes here is a check on the PERSON and not on the role:
	// compliance compares authenticated subjects, so two holders are two
	// signatures and one holder acting twice is refused across two requests.
	//
	// Conditional for the same reason the two above are — Grants is keyed by the
	// role STRING, and an unconditional entry would put a "" key in the map that
	// decides who may rewrite a mandate, which any token carrying an empty role
	// entry would match.
	//
	// IT CARRIES Read, and here that is not a convenience. An approver who cannot
	// read the portfolio's exposure and positions is signing a constraint change
	// against a book they cannot see — and the mandate they are approving is
	// evaluated against exactly that book.
	//
	// IT DOES NOT CARRY Approve, AND THAT SEPARATION IS THE ISSUE. validateAuth
	// refuses to start if the two role names collide: one signatory holding both
	// could give the second signature on relaxing a mandate and then the second
	// signature on the order that mandate would have refused, with each act
	// showing two names and nothing anywhere comparing the two records.
	if cfg.MandateRole != "" {
		grants[cfg.MandateRole] = []authz.Capability{authz.Read, authz.Mandate}
	}
	// THE AUDIT READER SEES WHO DID WHAT (#627): the tenant's audit history, a
	// decision's lineage, the SOC2 evidence pack and the chain attestation.
	//
	// Conditional for the same reason the three above are — Grants is keyed by the
	// role STRING, and an unconditional entry would put a "" key in the map that
	// decides who reads the compliance record.
	//
	// IT CARRIES Read, and unlike the three above that is not about competence to
	// judge: it is that Read is already universal here. Every caller who reaches
	// /v1 at all holds the baseline role, so listing it grants nothing new and
	// leaving it out would make this the only role in the map that cannot see the
	// system it is auditing.
	if cfg.AuditRole != "" {
		grants[cfg.AuditRole] = []authz.Capability{authz.Read, authz.Audit}
	}
	gwMux := authz.NewMux(grants, recorder)
	h.Routes(gwMux)
	o.Routes(gwMux)
	p.Routes(gwMux)
	// Absent when no control plane is configured — the routes are not registered at
	// all, rather than registered and forbidden. An operator hitting a 404 is being
	// told the truth (this gateway fronts no control plane); a 403 would say they
	// lacked a role, and they would go looking for the wrong thing.
	if ctl != nil {
		ctl.Routes(gwMux)
	}

	// Per-tenant quota policy (MT-01e): default budget + optional per-tenant
	// JSON overrides; metrics carry the tenant label.
	overrides, err := middleware.LoadQuotaOverrides(cfg.QuotasFile)
	if err != nil {
		logger.Error("api-gateway: quota overrides load failed", "err", err, "path", cfg.QuotasFile)
		return nil, err
	}
	limits := middleware.TenantLimits{
		Default: middleware.Limits{
			RatePerSec:  cfg.RateLimitPerSec,
			Burst:       cfg.RateLimitBurst,
			MaxInFlight: cfg.MaxInFlight,
		},
		Overrides: overrides,
	}
	gwMetrics := middleware.NewGatewayMetrics(obs.Registry)

	// Outermost first: negotiate version → verify signature → authenticate →
	// per-tenant request metrics → per-tenant quota (rate + admission, needs the
	// principal) → idempotency replay.
	chain := middleware.Chain(
		middleware.Version(),
		middleware.Signing(cfg.SigningSecret),
		middleware.Auth(authn, cfg.RequiredRole, logger),
		gwMetrics.Measure(),
		middleware.Quota(limits, gwMetrics),
		middleware.IdempotencyWith(idemClaims, idempotencyTTL, idempotencyMax),
	)

	mux := http.NewServeMux()
	mux.Handle("/v1/", chain(gwMux))
	mux.Handle("GET /metrics", obs.MetricsHandler())
	mux.HandleFunc("GET /openapi.json", gateway.OpenAPIHandler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux, nil
}

// dialRiskEngine creates the gRPC client connection to the risk-engine query
// server. mTLS (SEC-01b) when a SPIFFE socket is configured; plaintext for
// local/dev. grpc.NewClient is lazy — the TCP/TLS connection forms on first
// RPC, so a momentarily-unreachable upstream doesn't fail startup.
func dialRiskEngine(ctx context.Context, cfg config.Config, logger *slog.Logger) (*grpc.ClientConn, error) {
	return dialUpstream(ctx, cfg, cfg.RiskEngineAddr, logger)
}

// dialUpstream is the in-mesh dial every read upstream uses.
//
// ONE FUNCTION so a second upstream cannot arrive with a weaker posture. It was
// the risk-engine dialler's body until the OMS's order history became a second
// one (#399), and both carry portfolio-identifying data — a plaintext shortcut
// for either is a plaintext shortcut for the class.
func dialUpstream(ctx context.Context, cfg config.Config, addr string, logger *slog.Logger) (*grpc.ClientConn, error) {
	var opt grpc.DialOption
	if cfg.SPIFFESocket != "" {
		src, err := transport.NewSource(ctx, cfg.SPIFFESocket)
		if err != nil {
			return nil, err
		}
		opt = transport.ClientDialOption(src, transport.AuthorizeMesh())
		logger.Info("api-gateway: upstream mTLS enabled", "addr", addr, "socket", cfg.SPIFFESocket)
	} else {
		opt = grpc.WithTransportCredentials(insecure.NewCredentials())
		logger.Warn("api-gateway: upstream plaintext (no API_GATEWAY_SPIFFE_SOCKET)", "addr", addr)
	}
	return grpc.NewClient(addr, opt)
}

// operatorSPIFFEID is the identity the operator's control plane presents. It must
// match the namespace + ServiceAccount in infra/deploy/operator-deploy.yaml; a
// mismatch fails CLOSED (the dial is refused), which is the safe direction and is
// diagnosable from this gateway's own logs.
var operatorSPIFFEID = func() spiffeid.ID {
	id, err := transport.ServiceID("kanz-operator", "operator")
	if err != nil {
		panic("api-gateway: operator SPIFFE ID is not constructible: " + err.Error())
	}
	return id
}()

// dialOperator creates the connection to the operator's control plane (OPS-M2b).
//
// UNLIKE dialRiskEngine THIS REFUSES TO RUN PLAINTEXT, and unlike it the peer is
// pinned to ONE identity rather than AuthorizeMesh. Both differences are the same
// argument: this upstream provisions nodes and accepts exchange API keys, so dialing
// an impostor would mean handing a credential to whatever answered. Any mesh peer is
// too broad a set to trust with that, and no-TLS is not a degraded mode of it.
func dialOperator(ctx context.Context, cfg config.Config, logger *slog.Logger) (*grpc.ClientConn, error) {
	if cfg.SPIFFESocket == "" {
		return nil, errors.New("API_GATEWAY_OPERATOR_ADDR is set but API_GATEWAY_SPIFFE_SOCKET is " +
			"not: the control plane accepts exchange API keys and provisions nodes, and this " +
			"gateway will not carry that traffic unauthenticated")
	}
	src, err := transport.NewSource(ctx, cfg.SPIFFESocket)
	if err != nil {
		return nil, err
	}
	logger.Info("api-gateway: control-plane upstream pinned",
		"addr", cfg.OperatorAddr, "peer", operatorSPIFFEID.String())
	return grpc.NewClient(cfg.OperatorAddr,
		transport.ClientDialOption(src, transport.AuthorizeServices(operatorSPIFFEID)))
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// oidcAuthenticator adapts the canonical ctx-aware auth.Authenticator
// (AUTH-01a) to the gateway's ctx-less middleware.Authenticator seam, mapping
// the shared auth.Principal onto the gateway-local one. A background context is
// used since the middleware interface carries none — JWKS verification is
// offline once warm, so this only matters on a cold-cache fetch. The
// provider-unavailable vs bad-token distinction the package preserves collapses
// to a 401 here (the middleware maps every error to unauthenticated); a 503 on
// auth-backend outage is a noted follow-up.
type oidcAuthenticator struct{ a *auth.OIDCAuthenticator }

func (o oidcAuthenticator) Authenticate(token string) (*middleware.Principal, error) {
	p, err := o.a.Authenticate(context.Background(), token)
	if err != nil {
		return nil, err
	}
	return edgePrincipal(p), nil
}

// edgePrincipal maps the shared auth.Principal onto the gateway's edge type. It
// MUST populate every field of middleware.Principal, and test/arch's
// TestEveryAuthenticatorPopulatesTheWholePrincipal is what enforces that.
//
// THE FIELD IT USED TO OMIT WAS Portfolios, AND THAT WAS #225. This is the
// production authenticator — infra/deploy/api-gateway-deploy.yaml sets
// API_GATEWAY_OIDC_ISSUER, so every deployed request comes through here — and it
// dropped the caller's portfolio entitlement on the floor. The dev HS256 arm
// carried it and had a test; this arm had neither, so deleting the field from the
// dev arm failed a test while this arm shipped permanently in the deleted state.
// Downstream that meant the OMS refused every cancel and amend NOT_ENTITLED while
// admitting a submit into any portfolio in the tenant.
//
// A field-by-field copy rather than a struct embed or a shared type: the two
// Principals are deliberately different (the edge one carries no raw Claims bag
// and is reached by a different context accessor), and the guard can only read a
// literal.
func edgePrincipal(p *auth.Principal) *middleware.Principal {
	return &middleware.Principal{
		Subject:    p.Subject,
		Tenant:     p.Tenant,
		Roles:      p.Roles,
		Portfolios: p.Portfolios,
		IssuedAt:   p.IssuedAt,
	}
}

// tenantRiskEngineAddr derives a tenant's risk-engine gRPC target from the
// platform one, by suffixing the first hostname label with the tenant —
// `risk-engine.kanz-services.svc:9090` becomes
// `risk-engine-acme.kanz-services.svc:9090`.
//
// THE SAME RULE THE MANIFEST IS RENDERED BY. internal/tenantgen.WorkloadName is
// what names the per-tenant Service, so deriving the address any other way is a
// second spelling of one rule — and the failure is a dial against a Service that
// was rendered under a different name, which surfaces as an unavailable upstream
// rather than as anything pointing at the mismatch.
//
// A gRPC target is host:port with no scheme, so it is split directly rather than
// through url.Parse, which would read the host as a path.
func tenantRiskEngineAddr(base, tenant string) (string, error) {
	if base == "" {
		return "", fmt.Errorf("no platform risk-engine address to derive from")
	}
	host, port, err := net.SplitHostPort(base)
	if err != nil {
		// No port: the whole value is the host.
		host, port = base, ""
	}
	label, rest, found := strings.Cut(host, ".")
	if !found {
		label, rest = host, ""
	}
	if label == "" {
		return "", fmt.Errorf("%q has no hostname to suffix", base)
	}
	out := tenantgen.WorkloadName(label, tenant)
	if rest != "" {
		out += "." + rest
	}
	if port != "" {
		out = net.JoinHostPort(out, port)
	}
	return out, nil
}
