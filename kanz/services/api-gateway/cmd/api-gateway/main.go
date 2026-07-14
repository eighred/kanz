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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	querypb "github.com/kanz-eng/kanz-schemas-go/query/v1"

	"github.com/kanz-eng/kanz/pkg/auth"
	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/pkg/observability"
	"github.com/kanz-eng/kanz/pkg/transport"
	"github.com/kanz-eng/kanz/services/api-gateway/internal/config"
	"github.com/kanz-eng/kanz/services/api-gateway/internal/gateway"
	"github.com/kanz-eng/kanz/services/api-gateway/internal/middleware"
	"github.com/kanz-eng/kanz/services/api-gateway/internal/orders"
	"github.com/kanz-eng/kanz/services/api-gateway/internal/proxy"
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
		ServiceName: cfg.Source, ServiceVersion: version(), OTLPEndpoint: cfg.OTLPEndpoint, SampleRatio: 1,
	}, base)
	if err != nil {
		slog.Default().Error("observability init failed", "err", err)
		os.Exit(2)
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
		os.Exit(2)
	}
	conn, err := dialRiskEngine(ctx, cfg, logger)
	if err != nil {
		logger.Error("risk-engine dial failed", "err", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	handler := gateway.New(querypb.NewRiskQueryServiceClient(conn))

	// Order write surface (OMS-01d): publish order commands to the spine, with
	// the AUTH-01c forged-issuer guard on the producer. Nil publisher ⇒ the
	// write routes 503 (read-only gateway).
	ordersHandler, closeBus, err := buildOrders(ctx, cfg, logger)
	if err != nil {
		logger.Error("order write surface init failed", "err", err)
		os.Exit(1)
	}
	defer closeBus()

	// Phase-7 read surfaces (SVCWIRE-01b/c): wealth/datamaster/copilot routes
	// behind the same edge chain, forwarded over the SEC-01b mTLS mesh. With no
	// upstream addresses configured the backend is nil and the routes 503.
	proxyHandler := buildProxy(ctx, cfg, logger)

	var ready atomic.Bool
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           buildRouter(cfg, handler, ordersHandler, proxyHandler, obs, &ready, logger),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		ready.Store(true)
		logger.Info("api-gateway listening", "addr", cfg.Listen, "upstream", cfg.RiskEngineAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	ready.Store(false)
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
}

// buildOrders wires the OMS-01d write surface. With a NATS URL it dials the
// spine and returns a producer-backed handler (forged-issuer guard on); without
// one it returns a disabled handler whose routes 503. The returned func closes
// the bus client on shutdown.
func buildOrders(ctx context.Context, cfg config.Config, logger *slog.Logger) (*orders.Handler, func(), error) {
	if cfg.NATSURL == "" {
		logger.Warn("api-gateway: order write surface disabled (no API_GATEWAY_NATS_URL)")
		return orders.New(nil), func() {}, nil
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source})
	if err != nil {
		return nil, func() {}, err
	}
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:              cfg.Source,
		ProducerVersion:     version(),
		VerifyCommandIssuer: auth.VerifyCommandIssuer,
	})
	if err != nil {
		_ = client.Close()
		return nil, func() {}, err
	}
	logger.Info("api-gateway: order write surface enabled")
	return orders.New(producer), func() { _ = client.Close() }, nil
}

// buildProxy wires the Phase-7 read surfaces (SVCWIRE-01c). It collects the
// configured upstream base URLs and builds a mesh backend over an mTLS HTTP
// client (SEC-01b) when a SPIFFE socket is set, plaintext for local/dev. With no
// upstreams configured it returns a nil-backed handler whose routes 503 — the
// same disabled-surface shape as the order write surface.
func buildProxy(ctx context.Context, cfg config.Config, logger *slog.Logger) *proxy.Handler {
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
	if len(bases) == 0 {
		logger.Warn("api-gateway: Phase-7 read surfaces disabled (no upstream addresses)")
		return proxy.New(nil)
	}

	client := http.DefaultClient
	if cfg.SPIFFESocket != "" {
		src, err := transport.NewSource(ctx, cfg.SPIFFESocket)
		if err != nil {
			logger.Error("api-gateway: proxy SPIFFE source failed", "err", err)
			os.Exit(1)
		}
		client = &http.Client{
			Transport: &http.Transport{TLSClientConfig: transport.ClientTLSConfig(src, transport.AuthorizeMesh())},
			Timeout:   30 * time.Second,
		}
		logger.Info("api-gateway: Phase-7 upstreams mTLS enabled", "services", len(bases))
	} else {
		logger.Warn("api-gateway: Phase-7 upstreams plaintext (no API_GATEWAY_SPIFFE_SOCKET)")
	}
	return proxy.New(proxy.NewMeshBackend(bases, client))
}

// buildRouter wires the public probes/metrics/openapi (un-gated) and the /v1
// risk + order + Phase-7 read routes behind the edge middleware chain.
func buildRouter(cfg config.Config, h *gateway.Handler, o *orders.Handler, p *proxy.Handler, obs *observability.Provider, ready *atomic.Bool, logger *slog.Logger) http.Handler {
	gwMux := http.NewServeMux()
	h.Routes(gwMux)
	o.Routes(gwMux)
	p.Routes(gwMux)

	// One of these two arms always runs: config.Load refuses to return a Config
	// with neither an OIDC issuer nor a JWT secret, so the gateway cannot reach
	// here unauthenticated. There is no third arm, and middleware.Auth refuses
	// every request if a nil Authenticator ever reaches it anyway.
	var authn middleware.Authenticator
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
			os.Exit(2)
		}
		authn = oidcAuthenticator{oidc}
		logger.Info("api-gateway: OIDC authentication enabled", "issuer", cfg.OIDCIssuer)
	case cfg.JWTSecret != "":
		authn = middleware.NewJWTAuthenticator(cfg.JWTSecret)
		logger.Warn("api-gateway: using dev HS256 validator (set API_GATEWAY_OIDC_ISSUER for production)")
	}
	// Per-tenant quota policy (MT-01e): default budget + optional per-tenant
	// JSON overrides; metrics carry the tenant label.
	overrides, err := middleware.LoadQuotaOverrides(cfg.QuotasFile)
	if err != nil {
		logger.Error("api-gateway: quota overrides load failed", "err", err, "path", cfg.QuotasFile)
		os.Exit(2)
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
		middleware.Idempotency(time.Minute, 10_000),
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
	return mux
}

// dialRiskEngine creates the gRPC client connection to the risk-engine query
// server. mTLS (SEC-01b) when a SPIFFE socket is configured; plaintext for
// local/dev. grpc.NewClient is lazy — the TCP/TLS connection forms on first
// RPC, so a momentarily-unreachable upstream doesn't fail startup.
func dialRiskEngine(ctx context.Context, cfg config.Config, logger *slog.Logger) (*grpc.ClientConn, error) {
	var opt grpc.DialOption
	if cfg.SPIFFESocket != "" {
		src, err := transport.NewSource(ctx, cfg.SPIFFESocket)
		if err != nil {
			return nil, err
		}
		opt = transport.ClientDialOption(src, transport.AuthorizeMesh())
		logger.Info("api-gateway: upstream mTLS enabled", "socket", cfg.SPIFFESocket)
	} else {
		opt = grpc.WithTransportCredentials(insecure.NewCredentials())
		logger.Warn("api-gateway: upstream plaintext (no API_GATEWAY_SPIFFE_SOCKET)")
	}
	return grpc.NewClient(cfg.RiskEngineAddr, opt)
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
	return &middleware.Principal{Subject: p.Subject, Tenant: p.Tenant, Roles: p.Roles}, nil
}

// version is the service version stamped on telemetry. Hardcoded until the
// build injects a git SHA / semver (CICD-01a/c ldflags).
func version() string { return "dev" }
