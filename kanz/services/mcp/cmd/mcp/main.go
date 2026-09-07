// mcp binary entrypoint (#743): the agent-facing MCP READ plane.
//
// It exposes read-only tools over portfolio risk state to external agents,
// speaking JSON-RPC over one HTTP route behind the api-gateway. Every tool is
// tenant-scoped through internal/agentgate — the same authorization decision
// services/copilot runs, promoted to shared code when this plane became its
// second consumer rather than copied.
//
// WHAT THIS BINARY CANNOT DO, BY CONSTRUCTION. It places no orders, cancels
// nothing, amends nothing, holds no venue credential and dials no exchange. It
// speaks to exactly ONE upstream — the risk-engine's query.v1 read API — and
// test/arch/mcp_is_a_read_plane_test.go asserts on the import graph that none of
// the capital or venue surface is even reachable from here, because a tool that
// could do those things arrives as an import before it arrives as a handler.
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

	"github.com/eighred/kanz/internal/agentgate"
	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/platform/httpserver"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/pkg/observability"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/mcp/internal/config"
	"github.com/eighred/kanz/services/mcp/internal/riskread"
	"github.com/eighred/kanz/services/mcp/internal/server"
)

func main() {
	// The lifecycle lives in run() because os.Exit skips defers.
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
	fatal := lifecycle.NewFatal(stop)

	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
	obs, err := observability.New(ctx, observability.Config{
		ServiceName:    "mcp",
		ServiceVersion: version.String(),
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

	// THE AUTHORIZER, AND NO BUNDLE IS A REFUSAL.
	//
	// A missing policy file yields an authorizer that denies every action, so a
	// deployment that forgot the ConfigMap serves NO tenant's data rather than
	// every tenant's. That is the deny-by-default posture AGENTS.md requires of
	// this plane specifically, and it is the direction a misconfiguration must
	// fail in when the thing being served is somebody's book.
	authorizer, err := loadAuthorizer(cfg.PolicyFile, logger)
	if err != nil {
		logger.Error("mcp: policy bundle refused", "err", err, "file", cfg.PolicyFile)
		return 2
	}

	// The ONE upstream this plane reads.
	conn, err := dialRiskQuery(ctx, cfg, logger)
	if err != nil {
		logger.Error("risk query dial failed", "err", err, "addr", cfg.RiskQueryAddr)
		return 2
	}
	defer func() { _ = conn.Close() }()

	reader := riskread.New(querypb.NewRiskQueryServiceClient(conn))
	// The gate takes the SAME client as its OwnerResolver: "who owns this" and
	// "what does it say" must come from one source, or the plane can authorize
	// against one view of ownership and read from another.
	gate := agentgate.NewGate(authorizer, reader, logger)

	readiness := &server.Readiness{}

	// TWO LISTENERS, and the split is the security boundary (#409/#232).
	//
	// allow-observability-scrape must admit whatever port serves /metrics, and it
	// selects every pod in kanz-services. This plane decides which TENANT's state
	// an agent may read from a header the api-gateway injects, so sharing a port
	// would put that decision within reach of the monitoring namespace.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", obs.MetricsHandler())
	metricsSrv := httpserver.New(cfg.MetricsListen, metricsMux, httpserver.Standard())
	go func() {
		logger.Info("mcp metrics listening", "addr", cfg.MetricsListen)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed — this pod is now unmonitored", "err", err)
		}
	}()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutCtx)
	}()

	httpSrv := httpserver.New(cfg.Listen, server.New(readiness, gate, reader, logger), httpserver.Standard())
	go func() {
		logger.Info("mcp read plane listening", "addr", cfg.Listen, "upstream", cfg.RiskQueryAddr)
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

// loadAuthorizer builds the AUTH-01 authorizer from the policy bundle.
//
// AN ABSENT BUNDLE DENIES EVERYTHING rather than erroring at boot, and the two
// are different on purpose: a service that refuses to start is an outage an
// operator sees immediately, while a plane that starts and serves nothing is
// visible only when somebody asks it for something. This one is read-only and
// optional to the estate's operation, so the safe state is up-and-refusing —
// and it says so loudly at boot rather than leaving "no data" to be diagnosed.
func loadAuthorizer(path string, logger *slog.Logger) (auth.Authorizer, error) {
	if path == "" {
		logger.Warn("NO POLICY BUNDLE — MCP_POLICY_FILE is unset, so every tool call is DENIED. " +
			"This plane is up and serving nothing until a bundle is mounted")
		return denyAll{}, nil
	}
	policy, err := auth.LoadPolicyFile(path)
	if err != nil {
		return nil, err
	}
	return auth.NewPolicyAuthorizer(policy), nil
}

// denyAll is the no-bundle authorizer. It refuses with a code the gate reads as
// a GRANT refusal rather than an isolation one — the caller is told their token
// carries nothing here, which is true, instead of being told the resource does
// not exist, which would be a lie about another tenant's state.
type denyAll struct{}

func (denyAll) Authorize(context.Context, auth.Request) auth.Decision {
	return auth.Decision{
		Reason: "no policy bundle is configured for this MCP plane, so no role grants anything",
		Code:   auth.DenyNoGrant,
	}
}

// dialRiskQuery creates the gRPC connection to the risk-engine's query.v1 server.
// mTLS when a SPIFFE socket is configured, else plaintext for local dev — stated
// loudly, because this link carries one tenant's governed risk state to a plane
// whose job is deciding who may read it.
//
// grpc.NewClient is lazy: the connection forms on the first RPC, so a
// momentarily-unreachable engine does not fail this plane's startup.
func dialRiskQuery(ctx context.Context, cfg config.Config, logger *slog.Logger) (*grpc.ClientConn, error) {
	var opt grpc.DialOption
	if cfg.SPIFFESocket != "" {
		src, err := transport.NewSource(ctx, cfg.SPIFFESocket)
		if err != nil {
			return nil, err
		}
		opt = transport.ClientDialOption(src, transport.AuthorizeMesh())
		logger.Info("mcp: query.v1 mTLS enabled", "socket", cfg.SPIFFESocket)
	} else {
		logger.Warn("mcp: query.v1 is PLAINTEXT (no SPIFFE_ENDPOINT_SOCKET) — this link carries " +
			"governed risk state")
		opt = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	return grpc.NewClient(cfg.RiskQueryAddr, opt)
}
