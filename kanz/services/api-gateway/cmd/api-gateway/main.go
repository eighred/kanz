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

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	operatorpb "github.com/eighred/kanz/kanz-schemas-go/operator/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	"github.com/eighred/kanz/internal/lifecycle"
	"github.com/eighred/kanz/internal/version"
	"github.com/eighred/kanz/pkg/auth"
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

	handler := gateway.New(querypb.NewRiskQueryServiceClient(conn))

	// Order write surface (OMS-01d): publish order commands to the spine, with
	// the AUTH-01c forged-issuer guard on the producer. Nil publisher ⇒ the
	// write routes 503 (read-only gateway).
	ordersHandler, closeBus, err := buildOrders(ctx, cfg, logger)
	if err != nil {
		logger.Error("order write surface init failed", "err", err)
		return 2
	}
	defer closeBus()

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

	var ready atomic.Bool
	router, err := buildRouter(cfg, handler, ordersHandler, proxyHandler, ctlHandler, obs, &ready, logger)
	if err != nil {
		return 2
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      gatewayWriteTimeout,
		IdleTimeout:       gatewayIdleTimeout,
	}

	// The serve loop is the only thing that can die AFTER startup has completed
	// (rule 6, #266): lifecycle.Fatal lets the goroutine that observes the
	// failure hand it to run() without an os.Exit of its own, and stop()
	// cancelling ctx is what makes <-ctx.Done() below the join that guarantees
	// the write happens-before the read.
	fatal := lifecycle.NewFatal(stop)

	go func() {
		ready.Store(true)
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

// buildOrders wires the OMS-01d write surface. With a NATS URL it dials the
// spine and returns a producer-backed handler (forged-issuer guard on); without
// one it returns a disabled handler whose routes 503. The returned func closes
// the bus client on shutdown.
func buildOrders(ctx context.Context, cfg config.Config, logger *slog.Logger) (*orders.Handler, func(), error) {
	if cfg.NATSURL == "" {
		logger.Warn("api-gateway: order write surface disabled (no API_GATEWAY_NATS_URL)")
		return orders.New(nil), func() {}, nil
	}
	// SEC-M3: the production broker requires a client SVID; a nil TLSConfig is a
	// plaintext client it refuses at the handshake.
	mesh, err := transport.NewMesh(ctx, cfg.SPIFFESocket)
	if err != nil {
		return nil, func() {}, err
	}
	logger.Info("bus transport", "mtls", mesh.Enabled())
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: cfg.NATSURL, Name: cfg.Source, TLSConfig: mesh.Client})
	if err != nil {
		_ = mesh.Close()
		return nil, func() {}, err
	}
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source:              cfg.Source,
		ProducerVersion:     version.String(),
		VerifyCommandIssuer: auth.VerifyCommandIssuer,
	})
	if err != nil {
		_ = client.Close()
		_ = mesh.Close()
		return nil, func() {}, err
	}
	logger.Info("api-gateway: order write surface enabled")
	return orders.New(producer), func() { _ = client.Close(); _ = mesh.Close() }, nil
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
	if len(bases) == 0 {
		logger.Warn("api-gateway: Phase-7 read surfaces disabled (no upstream addresses)")
		return proxy.New(nil), nil
	}

	client := http.DefaultClient
	if cfg.SPIFFESocket != "" {
		src, err := transport.NewSource(ctx, cfg.SPIFFESocket)
		if err != nil {
			logger.Error("api-gateway: proxy SPIFFE source failed", "err", err)
			return nil, err
		}
		// NO Timeout FIELD, DELIBERATELY — it used to carry 30s and that was a bug of
		// the exact kind the TUI already paid for. http.Client.Timeout is enforced
		// independently of the request context, so it was a SECOND bound on the same
		// call and min(context, 30s) won invisibly: it capped /v1/ask at 30s underneath
		// the copilot's own five-minute completion budget, and it applied only on the
		// mTLS path, so dev and production were bounded differently for reasons nothing
		// stated. The one bound now lives on the call (proxy.forwardBudget), where the
		// route that needs a different budget can say so and a test can read it.
		// test/arch/probe_deadline_nesting_test.go fails the build if it comes back.
		client = &http.Client{
			Transport: &http.Transport{TLSClientConfig: transport.ClientTLSConfig(src, transport.AuthorizeMesh())},
		}
		logger.Info("api-gateway: Phase-7 upstreams mTLS enabled", "services", len(bases))
	} else {
		logger.Warn("api-gateway: Phase-7 upstreams plaintext (no API_GATEWAY_SPIFFE_SOCKET)")
	}
	return proxy.New(proxy.NewMeshBackend(bases, client)), nil
}

// buildRouter wires the public probes/metrics/openapi (un-gated) and the /v1
// risk + order + Phase-7 read routes behind the edge middleware chain.
func buildRouter(cfg config.Config, h *gateway.Handler, o *orders.Handler, p *proxy.Handler, ctl *control.Handler, obs *observability.Provider, ready *atomic.Bool, logger *slog.Logger) (http.Handler, error) {
	// EVERY /v1 ROUTE DECLARES WHAT IT TAKES TO REACH IT (SEC-M2).
	//
	// The gateway used to wrap all of /v1 in ONE role check, so `GET /v1/portfolios/{id}/
	// exposure` and `POST /v1/orders` were guarded identically: the token handed to an
	// analyst to look at exposure would submit an order to a live exchange.
	//
	// authz.Mux takes the capability as a required parameter of registration, so a route
	// cannot be added without deciding who may call it — the code would not compile. The
	// grants below are the only place a role becomes an authority.
	gwMux := authz.NewMux(authz.Grants{
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
	})
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
			return nil, err
		}
		authn = oidcAuthenticator{oidc}
		// The degraded-key posture, exported for as long as it lasts (#242).
		// Registered here rather than inside NewGatewayMetrics because it only
		// exists on this arm: the HS256 validator has no provider to lose.
		middleware.RegisterOIDCKeyGauge(obs.Registry, oidc.KeysUnrevalidated)
		logger.Info("api-gateway: OIDC authentication enabled", "issuer", cfg.OIDCIssuer)
	case cfg.JWTSecret != "":
		// Reachable only with API_GATEWAY_ALLOW_DEV_HS256=true — config.Load
		// refuses the arm otherwise, so this WARN now describes a deliberate
		// choice rather than an omission nobody noticed (#242).
		authn = middleware.NewJWTAuthenticator(cfg.JWTSecret)
		logger.Warn("api-gateway: using dev HS256 validator — a shared symmetric secret with no " +
			"revocation path (API_GATEWAY_ALLOW_DEV_HS256=true). Set API_GATEWAY_OIDC_ISSUER for production")
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
	return mux, nil
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
	}
}
