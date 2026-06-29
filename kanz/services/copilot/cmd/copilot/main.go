// copilot (AI analytics) binary entrypoint (COPILOT-01). It answers natural-
// language portfolio/risk questions over the GOVERNED read surface, powered by
// Claude (Opus 4.8), grounded strictly in cited governed data (ROI #42). Every
// answer is scoped to the authenticated AUTH-01 Principal and every tool call is
// authorized deny-by-default and logged to the AUDIT-01 observation stream.
//
// The Claude client and the governed query client default to the dependency-free
// in-tree seams (llm.StubModel / governed.StubClient). The real Opus 4.8
// anthropic-sdk-go client (model "claude-opus-4-8", adaptive thinking, the
// manual tool-use loop — see internal/llm doc) and the mTLS query.v1 client wire
// HERE at the composition root (DEBT-02), where pulling the LLM SDK does not
// burden the rest of the module.
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

	"github.com/kanz-eng/kanz/pkg/auth"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/services/copilot/internal/agent"
	"github.com/kanz-eng/kanz/services/copilot/internal/config"
	"github.com/kanz-eng/kanz/services/copilot/internal/governed"
	"github.com/kanz-eng/kanz/services/copilot/internal/llm"
	"github.com/kanz-eng/kanz/services/copilot/internal/retrieval"
	"github.com/kanz-eng/kanz/services/copilot/internal/server"
	"github.com/kanz-eng/kanz/services/copilot/internal/tools"
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
		ServiceName:    "copilot",
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

	// AUTH-01b authorizer (deny-by-default) wrapped in the AUTH-01d audited
	// authorizer so every copilot tool authorization — allow AND deny — lands on
	// the observation stream (SlogRecorder here; the bus-backed recorder wires in
	// the same way the rest of the platform records decisions).
	var inner auth.Authorizer = denyAll{}
	if cfg.PolicyPath != "" {
		policy, perr := auth.LoadPolicyFile(cfg.PolicyPath)
		if perr != nil {
			logger.Error("policy load failed", "err", perr)
			os.Exit(2)
		}
		inner = auth.NewPolicyAuthorizer(policy)
	}
	authz := auth.NewAuditedAuthorizer(inner, auth.NewSlogRecorder(logger), "copilot", logger)

	// Seams: dependency-free defaults; the real Opus 4.8 client + mTLS query
	// client replace these at deploy time.
	var model llm.Model = llm.NewStubModel()
	queryClient := governed.NewStubClient()
	registry := tools.NewRegistry(authz, queryClient, retrieval.IdentityCatalog{})
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

// denyAll is the boot-time authorizer when no policy is configured: it refuses
// everything (deny-by-default in the absence of a grant bundle), so a
// misconfigured deploy fails closed rather than open.
type denyAll struct{}

func (denyAll) Authorize(_ context.Context, _ auth.Request) auth.Decision {
	return auth.Decision{Allow: false, Reason: "no policy bundle configured"}
}

// version reads the build's VCS revision for the service-version label.
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
