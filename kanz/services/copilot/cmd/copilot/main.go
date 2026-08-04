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

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/auth"
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

	// AUTH-01b authorizer (deny-by-default) wrapped in the AUTH-01d audited
	// authorizer so every copilot tool authorization — allow AND deny — lands on
	// the observation stream (SlogRecorder here; the bus-backed recorder wires in
	// the same way the rest of the platform records decisions).
	var inner auth.Authorizer = denyAll{}
	if cfg.PolicyPath != "" {
		policy, perr := auth.LoadPolicyFile(cfg.PolicyPath)
		if perr != nil {
			logger.Error("policy load failed", "err", perr)
			return 2
		}
		inner = auth.NewPolicyAuthorizer(policy)
	}
	authz := auth.NewAuditedAuthorizer(inner, auth.NewSlogRecorder(logger), "copilot", logger)

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
	registry := tools.NewRegistry(authz, queryClient, catalog)
	cp := agent.New(model, registry)

	readiness := &server.Readiness{}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(readiness, logger, cp, server.WithMetrics(obs.MetricsHandler())),
		ReadHeaderTimeout: 5 * time.Second,
	}
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

func (denyAll) Authorize(_ context.Context, _ auth.Request) auth.Decision {
	return auth.Decision{Allow: false, Reason: "no policy bundle configured"}
}
