// copilot (AI analytics) binary entrypoint (COPILOT-01). It answers natural-
// language portfolio/risk questions over the GOVERNED read surface, powered by
// Claude (Opus 4.8), grounded strictly in cited governed data (ROI #42). Every
// answer is scoped to the authenticated AUTH-01 Principal and every tool call is
// authorized deny-by-default and logged to the AUDIT-01 observation stream.
//
// The Claude client and the governed query client default to the dependency-free
// in-tree seams (llm.StubModel / governed.StubClient). The real anthropic-sdk-go
// client — model = COPILOT_MODEL_ID (default claude-fable-5), adaptive thinking,
// the manual tool-use loop, the Fable-5 refusal + server-side fallback to
// claude-opus-4-8, streaming — is selected HERE at the composition root behind
// the `anthropic` build tag (model_anthropic.go), so only production images
// (built with `-tags anthropic`) pull the LLM SDK; the default build stays
// SDK-free. The mTLS query.v1 client wires here the same way (DEBT-02).
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

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/prometheus/client_golang/prometheus"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/authbus"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/copilot/internal/agent"
	"github.com/eighred/kanz/services/copilot/internal/config"
	"github.com/eighred/kanz/services/copilot/internal/governed"
	"github.com/eighred/kanz/services/copilot/internal/retrieval"
	"github.com/eighred/kanz/services/copilot/internal/server"
	"github.com/eighred/kanz/services/copilot/internal/tools"
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
		ServiceName:    "copilot",
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

	// AUTH-01d: every copilot tool authorization — allow AND deny — reaches the
	// observation stream. With no COPILOT_NATS_URL it falls back to the log, which
	// is where this service started and why #352 exists: a decision in a pod's
	// stdout is gone at the next rollout.
	recorder, closeRecorder, rerr := buildDecisionRecorder(ctx, cfg, obs.Registry, logger)
	if rerr != nil {
		logger.Error("decision recorder init failed", "err", rerr)
		return 2
	}
	defer closeRecorder()

	// AUTH-01b authorizer (deny-by-default) wrapped in the AUTH-01d audited
	// authorizer.
	var inner auth.Authorizer = denyAll{}
	if cfg.PolicyPath != "" {
		policy, perr := auth.LoadPolicyFile(cfg.PolicyPath)
		if perr != nil {
			logger.Error("policy load failed", "err", perr)
			return 2
		}
		inner = auth.NewPolicyAuthorizer(policy)
	}
	authz := auth.NewAuditedAuthorizer(inner, recorder, "copilot", logger)

	// Seams: newModel resolves COPILOT_PROVIDER against the adapters this binary
	// was linked with (#179). It FAILS rather than falling back — an unset or
	// unlinked provider is a refusal to start, not a quiet substitution.
	model, err := newModel(cfg, logger, obs.Registry)
	if err != nil {
		logger.Error("copilot cannot start", "err", err)
		return 2
	}

	// Governed query client (WIRE-02b): the real query.v1 gRPC client when a
	// risk-engine address is configured, else the dependency-free StubClient
	// (tests / local boot). The gRPC client reads the WIRE-02a owner_tenant +
	// source_position, feeding the deny-by-default authz gate + the citation seed.
	var queryClient governed.Client = governed.NewStubClient()
	if cfg.RiskQueryAddr != "" {
		conn, derr := dialRiskQuery(ctx, cfg, logger)
		if derr != nil {
			logger.Error("risk query dial failed", "err", derr)
			return 2
		}
		defer func() { _ = conn.Close() }()
		queryClient = governed.NewGRPCClient(querypb.NewRiskQueryServiceClient(conn))
		logger.Info("copilot governed reads: query.v1 gRPC", "addr", cfg.RiskQueryAddr)
	} else {
		logger.Warn("no COPILOT_RISK_QUERY_ADDR — governed reads use the in-memory stub")
	}
	// Citation catalog: the dependency-free IdentityCatalog by default; the LIN-01
	// lineage-backed LineageCatalog when a lineage address is configured (the mTLS
	// client wires here at deploy — the http.Client is injected so retrieval stays
	// SPIFFE-free, the SVCWIRE-01c stance). PARITY-04b.
	var catalog retrieval.Catalog = retrieval.IdentityCatalog{}
	if cfg.LineageAddr != "" {
		catalog = retrieval.NewLineageCatalog(nil, cfg.LineageAddr)
		logger.Info("copilot citations: lineage catalog", "addr", cfg.LineageAddr)
	}
	registry := tools.NewRegistry(authz, queryClient, catalog, logger)
	cp := agent.New(model, registry)

	readiness := &server.Readiness{}
	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, logger, cp, server.WithMetrics(obs.MetricsHandler())), httpserver.Standard())
	go func() {
		logger.Info("copilot listening", "addr", cfg.Listen, "model", cfg.ModelID)
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

// dialRiskQuery creates the gRPC client connection to the risk-engine query.v1
// server (WIRE-02b). mTLS (SEC-01b) when a SPIFFE socket is configured, else
// plaintext for local/dev. grpc.NewClient is lazy — the connection forms on the
// first RPC, so a momentarily-unreachable engine doesn't fail copilot startup.
func dialRiskQuery(ctx context.Context, cfg config.Config, logger *slog.Logger) (*grpc.ClientConn, error) {
	var opt grpc.DialOption
	if cfg.SPIFFESocket != "" {
		src, err := transport.NewSource(ctx, cfg.SPIFFESocket)
		if err != nil {
			return nil, err
		}
		opt = transport.ClientDialOption(src, transport.AuthorizeMesh())
		logger.Info("copilot: query.v1 mTLS enabled", "socket", cfg.SPIFFESocket)
	} else {
		opt = grpc.WithTransportCredentials(insecure.NewCredentials())
		logger.Warn("copilot: query.v1 plaintext (no COPILOT_SPIFFE_SOCKET)")
	}
	return grpc.NewClient(cfg.RiskQueryAddr, opt)
}

// denyAll is the boot-time authorizer when no policy is configured: it refuses
// everything (deny-by-default in the absence of a grant bundle), so a
// misconfigured deploy fails closed rather than open.
type denyAll struct{}

// authDecisionsLost counts AUTH-01d decisions that never reached the observation
// stream, by reason. A counter and not only a log line: a decision recorded to
// stdout is gone at the next rollout, so logging a DROPPED one to the same stdout
// reproduces the defect one layer down. Registered only when there is a bus —
// exported at zero with no broker it would read as "nothing was lost" when in
// truth nothing was ever published.
var authDecisionsLost = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "kanz_copilot_auth_decisions_lost_total",
	Help: "AUTH-01d authorization decisions that could not be published to the observation stream.",
}, []string{"reason"})

// buildDecisionRecorder returns the AUTH-01d recorder for the audited authorizer,
// plus a shutdown func.
//
// THE COPILOT HAD NO BUS AT ALL, which is why this was the last of #352's three
// sites. It publishes decisions and subscribes to nothing, so there is no
// consumer, no DLQ and no JetStream machinery here — one producer on one
// connection.
//
// Degradation is deliberate: with no COPILOT_NATS_URL it returns the slog
// recorder, exactly the behaviour before this change. Neither arm is a no-op — a
// service that cannot reach a broker must still record its authorization
// decisions somewhere.
func buildDecisionRecorder(ctx context.Context, cfg config.Config, reg prometheus.Registerer, logger *slog.Logger) (auth.DecisionRecorder, func(), error) {
	if cfg.NATSURL == "" {
		logger.Warn("no COPILOT_NATS_URL — AUTH-01d tool authorizations are recorded to the LOG ONLY, " +
			"so they do not survive a restart and cannot be queried beside the FACTs they justified")
		return auth.NewSlogRecorder(logger), func() {}, nil
	}
	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return nil, func() {}, err
	}
	logger.Info("bus transport", "mtls", mesh.Enabled())
	busMetrics := bus.NewBusMetrics(reg)
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: "copilot", TLSConfig: mesh.Client, Metrics: busMetrics})
	if err != nil {
		_ = mesh.Close()
		return nil, func() {}, err
	}
	// NO ProducerConfig.Tenant: the tenant is stamped per decision by pkg/authbus
	// from the deciding principal, so one analyst's tool authorizations are filed
	// under their own tenant rather than this deployment's.
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:          "copilot",
		ProducerVersion: version.String(),
		Metrics:         busMetrics,
	})
	if err != nil {
		_ = client.Close()
		_ = mesh.Close()
		return nil, func() {}, err
	}
	reg.MustRegister(authDecisionsLost)

	rec, err := authbus.NewBusRecorder(producer,
		authbus.WithFallbackTenant(cfg.Tenant),
		authbus.WithErrorHandler(func(err error) {
			authDecisionsLost.WithLabelValues("publish_error").Inc()
			logger.Error("an AUTH-01d decision could not be published — it exists only in this log line",
				"err", err)
		}),
		authbus.WithOverflowHandler(func(*observationpb.DecisionLog) {
			authDecisionsLost.WithLabelValues("queue_full").Inc()
			logger.Error("an AUTH-01d decision was DROPPED because the recorder queue is full — " +
				"the observation stream is now missing decisions this service did make")
		}),
	)
	if err != nil {
		_ = client.Close()
		_ = mesh.Close()
		return nil, func() {}, err
	}
	logger.Info("copilot: authorization decisions publish to the observation stream")
	// ORDER IS LOAD-BEARING: drain the recorder's queue BEFORE dropping the
	// connection it publishes over, or the decisions still in flight at shutdown —
	// precisely those made just before a rollout — are discarded.
	return rec, func() { rec.Close(); _ = client.Close(); _ = mesh.Close() }, nil
}

func (denyAll) Authorize(_ context.Context, _ auth.Request) auth.Decision {
	return auth.Decision{Allow: false, Reason: "no policy bundle configured"}
}
