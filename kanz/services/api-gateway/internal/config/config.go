package config

import (
	"errors"
	"fmt"
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/eighred/kanz/pkg/secret"
)

// Config is the api-gateway runtime configuration, sourced from the
// environment so it composes with the CSI/Vault secret mounts (SEC-01d).
type Config struct {
	Listen   string
	LogLevel slog.Level

	// Source is the gateway's service identity (logs, metrics, spans).
	Source string
	// OTLPEndpoint is the OTel collector for span export (OBS-01); empty ⇒
	// spans propagate but are not exported.
	OTLPEndpoint string

	// RiskEngineAddr is the gRPC target of the risk-engine query server
	// (API-01b). Required for the gateway to serve risk queries.
	RiskEngineAddr string
	// SPIFFESocket enables mTLS to the risk-engine when set (SEC-01b); empty ⇒
	// the upstream dial is plaintext (local/dev).
	SPIFFESocket string

	// OIDC* configure the real OIDC/JWKS authenticator (AUTH-01a). When
	// OIDCIssuer is set it takes precedence over JWTSecret — production auth.
	OIDCIssuer      string
	OIDCAudience    string
	OIDCJWKSURI     string // optional; discovered from the issuer when empty
	OIDCTenantClaim string // optional; defaults to "tenant"
	OIDCRolesClaim  string // optional; defaults to "roles"

	// RevocationsURI is identity's per-subject revocation feed (#532), the only
	// thing that makes disabling an account take effect on the session already
	// in flight rather than at the next login.
	//
	// REQUIRED ON THE OIDC ARM, and Load refuses without it. A gateway silently
	// running with no feed would look identical in every log and every dashboard
	// to one that had checked and found nobody revoked — this repository's
	// standing rule violated exactly, on the control whose whole purpose is to
	// be believed.
	//
	// IT IS NOT DERIVED FROM OIDCIssuer, deliberately. The issuer is a LOGICAL
	// name (https://identity.kanz.internal) that must match the token's `iss`
	// byte for byte and does not resolve; the reachable address is a cluster
	// URL, which is why OIDCJWKSURI is configured separately too. Deriving from
	// the issuer would point this at a host that does not exist and turn a
	// configuration mistake into a DNS error nobody traces back.
	RevocationsURI string

	// JWTSecret is the HS256 shared secret the bundled minimal JWT validator
	// verifies bearer tokens against (API-01d) — a dev-only stand-in used only
	// when no OIDC issuer is configured. One of OIDCIssuer or JWTSecret is
	// REQUIRED: with neither, Load refuses and the gateway does not start
	// (SEC-M1).
	JWTSecret string
	// AllowDevHS256 (API_GATEWAY_ALLOW_DEV_HS256) is the EXPLICIT opt-in without
	// which the HS256 arm is not a valid configuration at all (#242). A shared
	// symmetric secret is a credential every holder can forge with and has no
	// revocation path; reaching it must be a decision somebody wrote down, not
	// the consequence of leaving API_GATEWAY_OIDC_ISSUER unset.
	AllowDevHS256 bool
	// RequiredRole is the role a Principal must carry to reach any /v1 route
	// (deny-by-default). REQUIRED: without it, authentication admits every token
	// the issuer ever minted to every route, POST /v1/orders included, so Load
	// refuses (SEC-M1).
	RequiredRole string

	// TradeRole is the role a Principal must carry to reach a route that MOVES CAPITAL —
	// POST /v1/orders and POST /v1/orders/{id}/cancel (SEC-M2). REQUIRED, and it must
	// differ from RequiredRole: every authenticated caller carries the baseline role, so
	// making them the same would hand order entry to everyone who can read.
	TradeRole string

	// OperatorAddr is the gRPC target of the operator's control plane (OPS-M2b). EMPTY
	// ⇒ the /v1/control routes are NOT REGISTERED at all and the gateway serves trading
	// only — the operator surface is absent rather than present-and-forbidden, which is
	// the same shape the OMS uses for an unconfigured venue and the operator itself uses
	// for an unconfigured provisioner.
	OperatorAddr string

	// OMSReadAddr is the gRPC target of the OMS's order-history read surface
	// (#399). EMPTY disables it and GET /v1/portfolios/{id}/orders is then not
	// registered at all — an unregistered route says "not configured here",
	// which is true, while a registered one that always fails says "broken",
	// which is not. Same stance as OperatorAddr above.
	OMSReadAddr string
	// OperatorRole is the role a Principal must carry to reach a /v1/control route:
	// provisioning and draining nodes, and writing the exchange credentials the venue
	// adapters sign with. REQUIRED once OperatorAddr is set, and it must differ from
	// BOTH other roles — exposing the control plane without saying who may reach it is
	// the failure this pairing exists to prevent.
	OperatorRole string

	// RateLimitPerSec / RateLimitBurst configure the DEFAULT per-tenant token
	// bucket (API-01d). A non-positive rate disables rate limiting.
	RateLimitPerSec float64
	RateLimitBurst  int
	// MaxInFlight is the default per-tenant in-flight (concurrency) cap for
	// admission control (MT-01e). A non-positive value disables admission.
	MaxInFlight int
	// QuotasFile, when set, is a JSON map of tenant → {rate_per_sec, burst,
	// max_in_flight} overriding the defaults per tenant (MT-01e). The
	// ConfigMap-mounted policy-as-data shape (cf. AUTH-01b risk-authz.json).
	QuotasFile string

	// TrustedProxyHeader names the forwarded-for header the edge in front of this
	// gateway sets — "X-Forwarded-For" under ingress-nginx. Empty ⇒ the TCP peer
	// address is used and no header is honoured.
	//
	// IT IS WHAT THE PRE-AUTH LIMITER KEYS ON (#835). middleware.PreAuth bounds
	// how fast one SOURCE may fail to authenticate, and before authentication the
	// source is the only thing there is. Unset, every caller arriving through the
	// ingress controller shares the controller's own address as their key — safe,
	// and less precise. Set to a header from an UNTRUSTED peer it would be worse
	// than unset: a caller varies the value per request and every attempt lands in
	// a fresh bucket, so the limiter would stop existing while its metrics showed
	// a wide spread of well-behaved clients.
	TrustedProxyHeader string
	// TrustedProxies are the peer CIDRs (or bare addresses) permitted to set that
	// header — for ingress-nginx, the controller's pod CIDR. Both must be set
	// together or neither; Load refuses the half-configured state.
	TrustedProxies []string

	// SigningSecret, when set, requires every request to carry a valid HMAC
	// X-Signature over method+path+body (API-01d request signing). Empty ⇒
	// signing is not enforced.
	SigningSecret string

	// RedisURL backs the CROSS-POD idempotency claim. Empty ⇒ the per-pod
	// in-memory window, which is a real implementation and a weaker one.
	//
	// api-gateway runs replicas: 2, and the claim is what makes a repeated
	// Idempotency-Key at-most-once. Per-pod, a client retry that lands on the
	// other replica is not recognised as a retry — on /v1/orders that is a second
	// live order from one client intent. The posture is logged at startup so the
	// two cases do not look the same.
	//
	// The DSN carries a password, so it is a CSI/Vault file mount (SEC-01d) read
	// through pkg/secret, never a plaintext value in the pod spec.
	RedisURL string

	// NATSURL is the spine the order write surface (OMS-01d) publishes commands
	// to. Empty ⇒ the gateway is read-only (POST /v1/orders 503s).
	NATSURL string

	// WealthAddr / DataMasterAddr / CopilotAddr are the upstream base URLs of the
	// Phase-7 read services (SVCWIRE-01c), e.g.
	// "https://wealth.kanz-services:8080". Each empty ⇒ that surface 503s. The
	// gateway reaches them over the same SPIFFESocket mTLS as the risk-engine.
	WealthAddr     string
	DataMasterAddr string
	CopilotAddr    string
	// TVSyncAddr is the tv-sync Broker API — what a TradingView chart (and any
	// authorized human) reads to see the orders Kanz opened. It is exposed ONLY
	// through this gateway: tv-sync authenticates nothing and trusts the principal
	// header, so a direct route to it would let any caller name any tenant and read
	// that tenant's book. Empty ⇒ the /v1/broker/* routes 503.
	TVSyncAddr string

	// OptimizationAddr is the portfolio-construction service (#409).
	//
	// IT IS PROXIED FOR A SECURITY REASON, NOT FOR CONVENIENCE. The service takes
	// the issuer of a materialized order from its request body, and once
	// auto-publish is enabled that issuer is what the audit trail records as the
	// person who moved the capital. A caller-supplied string is exactly what the
	// AUTH-01c forged-issuer guard exists to prevent, so the service must never be
	// reachable except through here, where the principal is authenticated and
	// injected. Empty disables the routes rather than exposing them unauthenticated.
	OptimizationAddr string

	// MCPAddr is the agent-facing MCP read plane (#743), on its API listener
	// :8110 — never :8080, which serves that plane's /metrics and is admitted
	// namespace-wide by allow-observability-scrape.
	//
	// IT IS PROXIED FOR THE SAME SECURITY REASON TVSyncAddr IS. The plane
	// authenticates nobody: it reads the injected X-Kanz-Principal-* and scopes
	// every tool call to that tenant, refusing outright when the header is
	// absent. A direct route to it would let any caller name any tenant and read
	// that tenant's risk state through an agent. Empty ⇒ the /v1/mcp route 503s,
	// which is the state the whole plane shipped in (#762): no address, no route,
	// and no NetworkPolicy either way.
	MCPAddr string

	// RegulatoryAddr is the regulatory service's ESG SCREENING route, and only
	// that route (#751).
	//
	// It is the service's API listener :8103, not the :8083 that serves its
	// /metrics — allow-observability-scrape admits 8083 across every pod in the
	// namespace, which is why the filing routes moved off it (#765).
	//
	// THE FIVE FILING ROUTES ARE DELIBERATELY NOT PROXIED HERE. A filing is
	// signed and appends a link to the AUDIT-01 hash chain, so its capability is
	// a different question from a read route's — authz.Read would be wrong for
	// it, and deciding that while wiring something else is how a capability ends
	// up meaning nothing. The screen writes nothing and signs nothing.
	//
	// Empty ⇒ POST /v1/screening/esg 503s.
	RegulatoryAddr string

	// PerTenantUpstreams names the tenants that have their OWN rendered instances
	// of the MT-02 per-tenant services (internal/tenantgen.Services), so a read
	// on behalf of one is dialled at <service>-<tenant> instead of the shared
	// address (#668).
	//
	// EMPTY IS CORRECT for a deployment that has onboarded no tenant, and it is
	// the default. THE DANGEROUS VALUE IS A SHORT ONE: an onboarded tenant left
	// out is read from the __system__ instance, so a query for its NAV, cash or
	// ledger returns a 200 carrying the PLATFORM book's numbers — somebody else's
	// fills, presented as the caller's own. A tenant wrongly INCLUDED only 503s
	// against a Service that does not exist, which is loud and cheap.
	// test/arch/TestGatewayTenantUpstreamsMatchTheRenderedTenants compares this
	// against infra/deploy/tenants/ so the short list cannot ship.
	PerTenantUpstreams []string

	// AccountingAddr is the book of record (#415). It fronts the routes behind
	// authz.Fund — POST /v1/portfolios/{id}/cash-movements (#415), the custody
	// break queue (#962) and POST /v1/portfolios/{id}/reconcile (#1025) — and the
	// list has grown twice since it was written as "exactly one route", which is
	// why it names the issues rather than a count. Empty disables them, the same
	// shape as every other upstream here: a funding surface that is not configured
	// must 404 rather than answer.
	AccountingAddr string
	// FundRole is the role a Principal must carry to MOVE THE FUND'S OWN CAPITAL —
	// POST /v1/portfolios/{id}/cash-movements, which posts a subscription, a
	// redemption or a fee to the book of record (#535) — and, since #962/#1025, to
	// work the custody break queue and run the ad-hoc comparison behind it. Those
	// move no capital; they are middle-office fund operations by the same people,
	// and the reasoning for reusing this capability rather than minting a seventh
	// is written on the routes themselves.
	//
	// OPTIONAL, AND THE UNSET CASE IS THE DECISION. Empty ⇒ the route is NOT
	// REGISTERED at all, so a deployment that has named no funder answers 404 —
	// "there is no cash-movement surface here", which is true. This is the same
	// stance OperatorAddr and OMSReadAddr take above, and the one
	// services/identity/internal/server/server.go records for provisioning.
	//
	// THE ALTERNATIVE — REQUIRING IT UNCONDITIONALLY — IS WORSE, and not
	// marginally. It reads as the "fail loudly" answer, but what it fails is the
	// whole gateway: infra/deploy/api-gateway-deploy.yaml sets no fund role and no
	// accounting address, so a required variable turns "one route nobody can reach"
	// into "the platform's sole ingress will not start" — every read, every order,
	// every login — in exchange for arming a surface whose upstream is not even
	// wired. Fail loudly is about a misconfiguration that LOOKS HEALTHY; this one
	// cannot, because the route is absent and its absence is logged at boot.
	//
	// WHAT IT MUST NEVER BE IS THE THIRD ANSWER: registered, and granted to nobody.
	// That was the defect (#535) — a 403 to every principal that exists, which says
	// "you may not" when the truth is "nobody may, ever, in this deployment", and
	// is indistinguishable from a working control. validateAuth below closes the
	// other direction: once AccountingAddr IS set the role becomes REQUIRED, so the
	// funding surface cannot be exposed without saying who may reach it.
	FundRole string
	// ApproveRole is the role carrying authz.Approve — the second signature on an
	// act somebody else proposed (#539, #410 act one). EMPTY ⇒ the three override
	// routes are not registered, and datamaster's maker-checker workflow is
	// unreachable, which is the state this repository was in until #539.
	//
	// THERE IS DELIBERATELY NO "DataMasterAddr SET ⇒ THIS IS REQUIRED" RULE, and
	// that is the difference from FundRole above. AccountingAddr exists only to
	// serve the cash-movement route, so exposing one without the other is always a
	// mistake. DataMasterAddr also serves GET /v1/securities/{id}, /v1/prices/{id}
	// and /v1/exceptions — a read-only master-data deployment is a legitimate
	// posture, and forcing it to name an approver would make the pairing check a
	// lie that operators learn to satisfy with a placeholder.
	//
	// WHAT REPLACES IT IS A BOOT WARNING, not silence: main logs that the override
	// surface is absent whenever datamaster is wired and no approver is named. The
	// case that warning exists for is the dangerous one — datamaster armed with
	// DATAMASTER_REQUIRE_DUAL_CONTROL while this is unset, which holds every
	// override for a signature no one can give. That flag lives in another
	// service's environment and this process cannot read it, so a check here would
	// be guessing; a warning naming the consequence is what this side can honestly
	// say.
	ApproveRole string

	// ComplianceAddr is the compliance service's MANDATE-CHANGE listener (#562),
	// e.g. "https://compliance.kanz-services:8095". It is deliberately NOT the port
	// compliance serves /metrics and its probes on: allow-observability-scrape
	// admits the scraped port from the whole kanz-observability namespace, and a
	// pod there could otherwise send two self-chosen principals and hold BOTH
	// signatures on a mandate change.
	//
	// Empty ⇒ the three mandate routes have no upstream and 503, the same shape as
	// every other address here.
	ComplianceAddr string
	// MandateRole is the role carrying authz.Mandate — proposing or signing a
	// change to what governs a portfolio (#562, #410 act two).
	//
	// OPTIONAL, AND THE UNSET CASE IS THE DECISION, exactly as for FundRole: empty
	// ⇒ the routes are NOT REGISTERED, so a deployment that has named no mandate
	// signatory answers 404 rather than a 403 nobody could ever satisfy (#535).
	//
	// THE PAIRING RULE BELOW IS THE FUND ONE, NOT THE APPROVE ONE, and the
	// difference is which upstream serves what. DataMasterAddr also serves three
	// read routes, so a datamaster with no approver is a legitimate posture and a
	// required-role rule there would be a lie operators satisfy with a placeholder.
	// ComplianceAddr fronts the mandate surface and NOTHING else — that listener
	// has no other route on it — so setting it without naming a signatory exposes a
	// surface nobody can reach, which validateAuth refuses.
	MandateRole string

	// AuditAddr is the audit service's READ listener (#627) — :8102, NOT the
	// :8083 that serves /metrics. Empty ⇒ the six compliance-read routes are not
	// registered.
	//
	// THE PORT IS THE CONTROL. allow-observability-scrape admits the metrics port
	// namespace-wide, so pointing this at :8083 would front a surface the whole
	// monitoring plane can already reach directly with a principal it chose
	// itself, and the gateway would be decorating an open door.
	AuditAddr string
	// AuditRole is the role carrying authz.Audit — reading the record of who did
	// what. Empty with AuditAddr set is refused below (#535).
	//
	// IT MUST NOT BE THE BASELINE ROLE, and that is the one collision this
	// capability cannot survive: every authenticated caller holds the baseline, so
	// naming it here would serve every principal in the tenant the complete
	// history of every other principal's actions.
	AuditRole string
}

func Load() (Config, error) {
	// Both are resolved before the literal so a DECLARED-but-unreadable secret
	// mount stops Load here, rather than resolving to "". The local helper this
	// replaces answered a failed mount with the plaintext env var and then with
	// "", and "" means something different for each of these:
	//
	//   - JWTSecret "" is caught by validateAuth, but reported as "no
	//     authentication configured" — a broken Vault mount misdescribed as a
	//     deployment that never set one.
	//   - SigningSecret "" is caught by NOTHING: empty means request signing is
	//     not enforced, so a failed mount silently disarms it and the gateway
	//     reports a clean start.
	//
	// See pkg/secret.
	jwtSecret, err := secret.Read("API_GATEWAY_JWT_SECRET")
	if err != nil {
		return Config{}, err
	}
	signingSecret, err := secret.Read("API_GATEWAY_SIGNING_SECRET")
	if err != nil {
		return Config{}, err
	}
	allowDevHS256, err := parseBool("API_GATEWAY_ALLOW_DEV_HS256")
	if err != nil {
		return Config{}, err
	}
	redisURL, err := secret.Read("API_GATEWAY_REDIS_URL")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Listen:             env.Or("API_GATEWAY_LISTEN", ":8080"),
		LogLevel:           env.ParseLevelOr(env.Or("API_GATEWAY_LOG_LEVEL", "info"), slog.LevelInfo),
		Source:             env.Or("API_GATEWAY_SOURCE", "api-gateway"),
		OTLPEndpoint:       os.Getenv("API_GATEWAY_OTLP_ENDPOINT"),
		RiskEngineAddr:     os.Getenv("API_GATEWAY_RISK_ENGINE_ADDR"),
		OMSReadAddr:        os.Getenv("API_GATEWAY_OMS_READ_ADDR"),
		SPIFFESocket:       os.Getenv("API_GATEWAY_SPIFFE_SOCKET"),
		RedisURL:           redisURL,
		OIDCIssuer:         os.Getenv("API_GATEWAY_OIDC_ISSUER"),
		OIDCAudience:       os.Getenv("API_GATEWAY_OIDC_AUDIENCE"),
		OIDCJWKSURI:        os.Getenv("API_GATEWAY_OIDC_JWKS_URI"),
		RevocationsURI:     os.Getenv("API_GATEWAY_REVOCATIONS_URI"),
		OIDCTenantClaim:    os.Getenv("API_GATEWAY_OIDC_TENANT_CLAIM"),
		OIDCRolesClaim:     os.Getenv("API_GATEWAY_OIDC_ROLES_CLAIM"),
		JWTSecret:          jwtSecret,
		AllowDevHS256:      allowDevHS256,
		RequiredRole:       os.Getenv("API_GATEWAY_REQUIRED_ROLE"),
		TradeRole:          os.Getenv("API_GATEWAY_TRADE_ROLE"),
		OperatorAddr:       os.Getenv("API_GATEWAY_OPERATOR_ADDR"),
		OperatorRole:       os.Getenv("API_GATEWAY_OPERATOR_ROLE"),
		RateLimitPerSec:    parseFloat(os.Getenv("API_GATEWAY_RATE_LIMIT_PER_SEC")),
		RateLimitBurst:     parseInt(os.Getenv("API_GATEWAY_RATE_LIMIT_BURST")),
		MaxInFlight:        parseInt(os.Getenv("API_GATEWAY_MAX_IN_FLIGHT")),
		QuotasFile:         os.Getenv("API_GATEWAY_QUOTAS_FILE"),
		TrustedProxyHeader: os.Getenv("API_GATEWAY_TRUSTED_PROXY_HEADER"),
		TrustedProxies:     env.SplitList(os.Getenv("API_GATEWAY_TRUSTED_PROXIES")),
		SigningSecret:      signingSecret,
		NATSURL:            os.Getenv("API_GATEWAY_NATS_URL"),
		WealthAddr:         os.Getenv("API_GATEWAY_WEALTH_ADDR"),
		DataMasterAddr:     os.Getenv("API_GATEWAY_DATAMASTER_ADDR"),
		CopilotAddr:        os.Getenv("API_GATEWAY_COPILOT_ADDR"),
		TVSyncAddr:         os.Getenv("API_GATEWAY_TV_SYNC_ADDR"),
		OptimizationAddr:   os.Getenv("API_GATEWAY_OPTIMIZATION_ADDR"),
		MCPAddr:            os.Getenv("API_GATEWAY_MCP_ADDR"),
		RegulatoryAddr:     os.Getenv("API_GATEWAY_REGULATORY_ADDR"),
		AccountingAddr:     os.Getenv("API_GATEWAY_ACCOUNTING_ADDR"),
		PerTenantUpstreams: env.SplitList(os.Getenv("API_GATEWAY_PER_TENANT_UPSTREAMS")),
		FundRole:           os.Getenv("API_GATEWAY_FUND_ROLE"),
		ApproveRole:        os.Getenv("API_GATEWAY_APPROVE_ROLE"),
		ComplianceAddr:     os.Getenv("API_GATEWAY_COMPLIANCE_ADDR"),
		MandateRole:        os.Getenv("API_GATEWAY_MANDATE_ROLE"),
		AuditAddr:          os.Getenv("API_GATEWAY_AUDIT_ADDR"),
		AuditRole:          os.Getenv("API_GATEWAY_AUDIT_ROLE"),
	}
	if err := cfg.validateAuth(); err != nil {
		return Config{}, err
	}
	// Naming a header with nobody trusted to send it, or the reverse, silently
	// disables it — and the operator who set one of the two believes the pre-auth
	// limiter is keyed on the real caller when it is not. Refuse rather than run in
	// a state that reads as configured and honours nothing. Same rule, same
	// wording, as web-bff's (the other clientip caller).
	if (cfg.TrustedProxyHeader == "") != (len(cfg.TrustedProxies) == 0) {
		return Config{}, errors.New("API_GATEWAY_TRUSTED_PROXY_HEADER and API_GATEWAY_TRUSTED_PROXIES " +
			"must be set together or not at all — one without the other reads as configured and " +
			"honours nothing, and the pre-auth limiter would key on the proxy instead of the caller")
	}
	return cfg, nil
}

// validateAuth refuses to hand back a gateway that cannot say no.
//
// This process is the platform's sole identity authority: every /v1 route,
// POST /v1/orders included, is reachable only through it, and tv-sync trusts
// the principal header it injects. Deny-by-default is the house rule at every
// other gate on this platform (the mandate gate, the venue-account gate, RLS),
// and it is enforced here the same way — by refusing to run, not by logging a
// warning that scrolls past.
func (c Config) validateAuth() error {
	if c.OIDCIssuer == "" && c.JWTSecret == "" {
		return errors.New("api-gateway: no authentication configured — set API_GATEWAY_OIDC_ISSUER " +
			"(production, OIDC/JWKS) or API_GATEWAY_JWT_SECRET with API_GATEWAY_ALLOW_DEV_HS256=true " +
			"(dev, HS256). The gateway will not serve /v1/* — including POST /v1/orders — to " +
			"unauthenticated callers")
	}
	// #242. THE DEV CREDENTIAL MUST BE REACHED ON PURPOSE, NEVER BY OMISSION.
	//
	// Until this check existed, API_GATEWAY_JWT_SECRET alone was a complete and
	// silent authentication configuration: a staging or DR gateway brought up
	// from a partial copy of the production environment — one where the OIDC
	// issuer had been dropped and the dev secret had not — started, logged a
	// WARN nobody reads, and authenticated the whole estate against a symmetric
	// HMAC key. Nothing in the config distinguished that from a deployment that
	// meant it, which is this repository's standing rule violated exactly:
	// "nothing configured" and "checked, and fine" must never look the same.
	//
	// The secret is the wrong thing to gate on. A secret is a value, and a value
	// arrives by inheritance, by a copied ConfigMap, by a Vault path that still
	// resolves; a SEPARATE, purpose-named boolean has to be typed by somebody
	// who read what it turns on. That is the whole difference between a dev
	// credential in a dev estate and a dev credential in a real one.
	//
	// Refusing to start rather than warning is the same stance as every other
	// gate in this function, and for the same reason: a WARN at 03:00 in a
	// rollout log is discovered by the incident, and a refusal is discovered by
	// whoever deployed it, immediately.
	if c.OIDCIssuer == "" && !c.AllowDevHS256 {
		return errors.New("api-gateway: API_GATEWAY_JWT_SECRET is set but API_GATEWAY_ALLOW_DEV_HS256 " +
			"is not true. The HS256 validator is a DEV credential: a shared symmetric secret every " +
			"holder can forge tokens with, with no revocation path and no identity provider behind " +
			"it. It must be switched on deliberately, not reached by leaving API_GATEWAY_OIDC_ISSUER " +
			"unset. For anything real, configure OIDC/JWKS instead (#242)")
	}
	if c.RequiredRole == "" {
		return errors.New("api-gateway: no authorization configured — set API_GATEWAY_REQUIRED_ROLE. " +
			"Authentication alone admits every token the issuer ever minted, for any client and any " +
			"purpose, to every /v1 route — POST /v1/orders included")
	}
	// SEC-M2. One role for everything meant any principal who could READ could TRADE: the
	// token handed to an analyst to look at exposure would submit an order to a live
	// exchange. The gateway must be told who may move capital, and it will not guess.
	//
	// Leaving TradeRole empty would fail closed (nobody trades) — but that is a trading
	// outage dressed as a control, and it would be discovered by an order that did not go
	// out. Say who trades, out loud, in the deployment.
	if c.TradeRole == "" {
		return errors.New("api-gateway: no trade authority configured — set API_GATEWAY_TRADE_ROLE. " +
			"Without it there is one role for everything, and the token you give an analyst to read " +
			"exposure also submits orders against a live exchange (SEC-M2)")
	}
	if c.TradeRole == c.RequiredRole {
		return errors.New("api-gateway: API_GATEWAY_TRADE_ROLE must differ from API_GATEWAY_REQUIRED_ROLE. " +
			"EVERY authenticated caller carries the baseline role — making it the trade role hands " +
			"order entry to everyone who can read, which is the exact failure SEC-M2 exists to end")
	}
	// OPS-M2b. Only checked when the control plane is actually exposed: with no
	// OperatorAddr the /v1/control routes are never registered, so there is nothing to
	// authorize and demanding a role for it would be config for an absent feature.
	//
	// Once it IS exposed, the role is required and must be its own. These routes
	// provision and drain nodes and write the exchange credentials the venue adapters
	// sign with; reusing the baseline role would hand that to every authenticated
	// caller, and reusing the trade role would hand it to every trader.
	if c.OperatorAddr != "" {
		if c.OperatorRole == "" {
			return errors.New("api-gateway: API_GATEWAY_OPERATOR_ADDR is set but " +
				"API_GATEWAY_OPERATOR_ROLE is not. The control plane provisions and drains nodes " +
				"and writes exchange API credentials — exposing it without saying who may reach it " +
				"is not a default, it is an omission (OPS-M2b)")
		}
		if c.OperatorRole == c.RequiredRole {
			return errors.New("api-gateway: API_GATEWAY_OPERATOR_ROLE must differ from " +
				"API_GATEWAY_REQUIRED_ROLE. EVERY authenticated caller carries the baseline role — " +
				"making it the operator role hands node provisioning and exchange-credential writes " +
				"to everyone who can read")
		}
		if c.OperatorRole == c.TradeRole {
			return errors.New("api-gateway: API_GATEWAY_OPERATOR_ROLE must differ from " +
				"API_GATEWAY_TRADE_ROLE. Operating the estate and moving capital are different " +
				"authorities in BOTH directions: a trader has no business rotating the credentials " +
				"their orders are signed with, and an operator draining a node has no business " +
				"submitting orders")
		}
	}
	// #535. THE FUNDING SURFACE MUST NOT BE EXPOSED WITH NO ONE ABLE TO REACH IT.
	//
	// Only checked when the book of record is actually fronted: with no
	// AccountingAddr the cash-movement route has no upstream, and it is not
	// registered either (the fund role is what registers it), so demanding a role
	// would be config for an absent feature — the OperatorAddr pairing above,
	// exactly.
	//
	// The state this closes is the one #535 found: authz.Fund declared, the route
	// registered, and no role carrying the capability, so every principal that
	// exists got a 403. A capability granted to nobody is not a strict control, it
	// is an outage of that capability wearing one — and from outside the two are
	// identical.
	if c.AccountingAddr != "" && c.FundRole == "" {
		return errors.New("api-gateway: API_GATEWAY_ACCOUNTING_ADDR is set but API_GATEWAY_FUND_ROLE " +
			"is not. That route posts subscriptions, redemptions and fees to the book of record, and " +
			"with no role carrying authz.Fund it would answer 403 to EVERY principal that exists — " +
			"which reads as a working control and is a total outage of the capability (#535). Name " +
			"the funder, or unset API_GATEWAY_ACCOUNTING_ADDR and the route is not registered at all")
	}
	// The collisions, on the same argument as the operator role's: a shared name
	// silently merges two authorities that exist to be separate. THE TRADE ONE IS
	// THE POINT — the person who can move money is never the person who trades it,
	// the oldest segregation of duties in fund operations, and one credential
	// holding both can bring the fund's cash in and spend it.
	if c.FundRole != "" {
		switch {
		case c.FundRole == c.RequiredRole:
			return errors.New("api-gateway: API_GATEWAY_FUND_ROLE must differ from " +
				"API_GATEWAY_REQUIRED_ROLE. EVERY authenticated caller carries the baseline role — " +
				"making it the fund role lets everyone who can read post a redemption against the IBOR")
		case c.FundRole == c.TradeRole:
			return errors.New("api-gateway: API_GATEWAY_FUND_ROLE must differ from " +
				"API_GATEWAY_TRADE_ROLE. Funding and trading are different authorities in BOTH " +
				"directions: a trader who moves capital all day has no business deciding how much " +
				"capital the fund holds, and a funder has no business submitting an order. Collapsing " +
				"them gives one credential the power to both bring cash in and spend it, which is the " +
				"single control every auditor of a fund asks about first")
		case c.FundRole == c.OperatorRole:
			return errors.New("api-gateway: API_GATEWAY_FUND_ROLE must differ from " +
				"API_GATEWAY_OPERATOR_ROLE. Operating the estate is draining nodes and rotating " +
				"credentials; it touches no fund capital, and an SRE holding it must not be able to " +
				"move the fund's cash")
		}
	}
	// THE APPROVER COLLISIONS, AND THEY ARE THE STRICTEST SET ON THIS CONFIG (#539).
	//
	// Every other capability tolerates being held alongside another as a policy
	// choice somebody could defend. This one cannot, because a second signature
	// from a role that already holds the authority to act alone is not a control at
	// all — and it FAILS SILENTLY. datamaster still refuses self-approval, so the
	// trail shows two distinct people; a shared role name means any two holders
	// satisfy it, and the record an auditor reads is indistinguishable from real
	// four-eyes.
	if c.ApproveRole != "" {
		switch {
		case c.ApproveRole == c.RequiredRole:
			return errors.New("api-gateway: API_GATEWAY_APPROVE_ROLE must differ from " +
				"API_GATEWAY_REQUIRED_ROLE. EVERY authenticated caller carries the baseline role — " +
				"making it the approver role means every user in the tenant is a signatory, so any " +
				"two of them clear each other's overrides and the second signature means nothing")
		case c.ApproveRole == c.TradeRole:
			return errors.New("api-gateway: API_GATEWAY_APPROVE_ROLE must differ from " +
				"API_GATEWAY_TRADE_ROLE. This is the collision that matters: it hands every trader " +
				"the second signature on every other trader's proposal, so two people on the same " +
				"desk satisfy a control that exists to put a different function in the loop. Nothing " +
				"downstream can detect it — the approver is a different SUBJECT, which is all " +
				"internal/dualcontrol checks")
		case c.ApproveRole == c.OperatorRole:
			return errors.New("api-gateway: API_GATEWAY_APPROVE_ROLE must differ from " +
				"API_GATEWAY_OPERATOR_ROLE. Operating the estate is draining nodes and rotating " +
				"credentials; approving an override decides the marks the book is valued at. An SRE " +
				"is not the second pair of eyes on a valuation")
		case c.ApproveRole == c.FundRole:
			return errors.New("api-gateway: API_GATEWAY_APPROVE_ROLE must differ from " +
				"API_GATEWAY_FUND_ROLE. Both are senior authorities and that is exactly why they " +
				"drift together: the approver would become a seniority badge rather than a separate " +
				"function, and the person who moves the fund's cash would clear the prices the fund " +
				"is valued at")
		}
	}
	// #562. THE MANDATE SURFACE MUST NOT BE EXPOSED WITH NO ONE ABLE TO REACH IT.
	//
	// The FundRole pairing, and it applies here for the same reason it does there
	// and not for datamaster: compliance's mandate listener serves the three
	// mandate routes and nothing else, so fronting it without naming a signatory
	// exposes a surface every principal that exists gets a 403 from — a total
	// outage of the capability wearing a strict control's costume (#535).
	if c.ComplianceAddr != "" && c.MandateRole == "" {
		return errors.New("api-gateway: API_GATEWAY_COMPLIANCE_ADDR is set but " +
			"API_GATEWAY_MANDATE_ROLE is not. Those routes change the mandate every order in a " +
			"portfolio is checked against, and with no role carrying authz.Mandate they would " +
			"answer 403 to EVERY principal that exists — which reads as a working control and is " +
			"a total outage of the capability (#535). Name the signatory, or unset " +
			"API_GATEWAY_COMPLIANCE_ADDR and the routes are not registered at all (#562)")
	}
	// THE AUDIT READ SURFACE (#627), refused on the same #535 grounds.
	if c.AuditAddr != "" && c.AuditRole == "" {
		return errors.New("api-gateway: API_GATEWAY_AUDIT_ADDR is set but API_GATEWAY_AUDIT_ROLE " +
			"is not. Those routes serve the tenant's audit history, a decision's lineage and the " +
			"SOC2 evidence pack, and with no role carrying authz.Audit they would answer 403 to " +
			"EVERY principal that exists — a total outage of the capability wearing a strict " +
			"control's costume (#535). Name the audit reader, or unset API_GATEWAY_AUDIT_ADDR and " +
			"the routes are not registered at all")
	}
	// THE ONE COLLISION THIS CAPABILITY CANNOT SURVIVE. Unlike the trade/operator
	// pairs, a deployment MAY legitimately give its operators or its approvers the
	// audit role — reading the record is not acting in it, and a firm small enough
	// to have one compliance officer should not be forced to invent a second
	// person. The baseline is different in kind: every authenticated caller holds
	// it, so this line would not grant an authority, it would delete one.
	if c.AuditRole != "" && c.AuditRole == c.RequiredRole {
		return errors.New("api-gateway: API_GATEWAY_AUDIT_ROLE must differ from " +
			"API_GATEWAY_REQUIRED_ROLE. EVERY authenticated caller carries the baseline role, so " +
			"naming it here serves every principal in the tenant the complete record of every " +
			"OTHER principal's actions — which trader was refused by the pre-trade gate, who " +
			"overrode a price, who signed a mandate change. Name a role only the auditors hold")
	}
	// THE MANDATE COLLISIONS, AND THE APPROVE ONE IS THE POINT.
	//
	// Every other collision here merges two authorities that were meant to be
	// separate. This one merges two HALVES OF ONE ESCALATION: relax the constraint
	// that would have refused an order, then clear the order it would have refused.
	// With one role holding both, the second signature on the mandate change and
	// the second signature on the held order come from the same person — each act
	// still shows two names, each is still refused as a self-approval, and nothing
	// anywhere compares the two records. That is #562's whole argument for a
	// separate capability, enforced as configuration the process will not start
	// without rather than as a convention.
	if c.MandateRole != "" {
		switch {
		case c.MandateRole == c.RequiredRole:
			return errors.New("api-gateway: API_GATEWAY_MANDATE_ROLE must differ from " +
				"API_GATEWAY_REQUIRED_ROLE. EVERY authenticated caller carries the baseline role — " +
				"making it the mandate role means any two users in the tenant can rewrite what " +
				"governs a portfolio, and the pre-trade compliance gate then enforces whatever they " +
				"agreed between them")
		case c.MandateRole == c.TradeRole:
			return errors.New("api-gateway: API_GATEWAY_MANDATE_ROLE must differ from " +
				"API_GATEWAY_TRADE_ROLE. A trader who can change the mandate does not need to break " +
				"the pre-trade gate, only to widen it — and two traders on one desk would then be " +
				"the whole control on what the fund may hold")
		case c.MandateRole == c.ApproveRole:
			return errors.New("api-gateway: API_GATEWAY_MANDATE_ROLE must differ from " +
				"API_GATEWAY_APPROVE_ROLE. This is the collision #562 exists to prevent: one " +
				"signatory would give the second signature on relaxing a mandate AND the second " +
				"signature on the order that mandate would have refused. Both acts show two names, " +
				"both pass every self-approval check, and nothing compares the two records — the " +
				"escalation is invisible in exactly the trail an auditor would read")
		case c.MandateRole == c.OperatorRole:
			return errors.New("api-gateway: API_GATEWAY_MANDATE_ROLE must differ from " +
				"API_GATEWAY_OPERATOR_ROLE. Operating the estate is draining nodes and rotating " +
				"credentials; it decides nothing about what the fund may hold, and an SRE is not the " +
				"investment committee")
		case c.MandateRole == c.FundRole:
			return errors.New("api-gateway: API_GATEWAY_MANDATE_ROLE must differ from " +
				"API_GATEWAY_FUND_ROLE. Both are senior authorities and that is exactly why they " +
				"drift together: the person who brings the fund's capital in would also decide what " +
				"it may be invested in, with nobody else in the loop")
		}
	}
	// #532. AN AUTHENTICATION PATH WITH NO REVOCATION PATH IS ONE THE HOLDER OF A
	// STOLEN TOKEN OUTLIVES.
	//
	// Disabling an account used to stop the NEXT login and nothing else: the
	// gateway reads no account state, so an offboarded trader's token kept
	// working for its full remaining lifetime. Identity's feed is what closes
	// that, and a deployment omitting it gets an authentication path
	// indistinguishable from the one that had the gap — no warning, no metric,
	// nothing to notice.
	//
	// Only on the OIDC arm. The HS256 arm is dev-only by construction (#242) and
	// already declares itself as having no revocation path; requiring a feed there
	// would demand an identity service a dev rig deliberately does not run.
	//
	// LAST, so an otherwise-incomplete config still names the role it is missing
	// first. An operator who has set neither this nor API_GATEWAY_REQUIRED_ROLE
	// should be told about the role, fix it, and then be told about this — two
	// clear refusals beat one that answers for a setting they had not reached yet.
	if c.OIDCIssuer != "" && c.RevocationsURI == "" {
		return errors.New("api-gateway: API_GATEWAY_REVOCATIONS_URI is unset. Without identity's " +
			"revocation feed this gateway cannot tell a disabled account from an active one, so " +
			"disabling an account stops the next login and leaves the token already in the holder's " +
			"hands working until it expires — including one exfiltrated from a log or a compromised " +
			"pod. Point it at identity's /revocations (#532)")
	}
	return nil
}
func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func parseInt(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// parseBool reads a boolean env var. Unset or empty is false; anything
// strconv.ParseBool cannot read is an ERROR rather than a silent false.
//
// The silent-false version is what makes an opt-in useless. An operator who
// writes API_GATEWAY_ALLOW_DEV_HS256=yes has stated an intent as clearly as one
// who writes true; swallowing the parse failure would refuse the gateway with a
// message telling them to set a variable they can see they have already set,
// and the next attempt is usually to delete the check.
func parseBool(key string) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("api-gateway: %s=%q is not a boolean (use true or false)", key, raw)
	}
	return v, nil
}
