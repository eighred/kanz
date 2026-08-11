package config

import (
	"errors"
	"fmt"
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

	// SigningSecret, when set, requires every request to carry a valid HMAC
	// X-Signature over method+path+body (API-01d request signing). Empty ⇒
	// signing is not enforced.
	SigningSecret string

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

	cfg := Config{
		Listen:          envOr("API_GATEWAY_LISTEN", ":8080"),
		LogLevel:        parseLevel(envOr("API_GATEWAY_LOG_LEVEL", "info")),
		Source:          envOr("API_GATEWAY_SOURCE", "api-gateway"),
		OTLPEndpoint:    os.Getenv("API_GATEWAY_OTLP_ENDPOINT"),
		RiskEngineAddr:  os.Getenv("API_GATEWAY_RISK_ENGINE_ADDR"),
		OMSReadAddr:     os.Getenv("API_GATEWAY_OMS_READ_ADDR"),
		SPIFFESocket:    os.Getenv("API_GATEWAY_SPIFFE_SOCKET"),
		OIDCIssuer:      os.Getenv("API_GATEWAY_OIDC_ISSUER"),
		OIDCAudience:    os.Getenv("API_GATEWAY_OIDC_AUDIENCE"),
		OIDCJWKSURI:     os.Getenv("API_GATEWAY_OIDC_JWKS_URI"),
		OIDCTenantClaim: os.Getenv("API_GATEWAY_OIDC_TENANT_CLAIM"),
		OIDCRolesClaim:  os.Getenv("API_GATEWAY_OIDC_ROLES_CLAIM"),
		JWTSecret:       jwtSecret,
		AllowDevHS256:   allowDevHS256,
		RequiredRole:    os.Getenv("API_GATEWAY_REQUIRED_ROLE"),
		TradeRole:       os.Getenv("API_GATEWAY_TRADE_ROLE"),
		OperatorAddr:    os.Getenv("API_GATEWAY_OPERATOR_ADDR"),
		OperatorRole:    os.Getenv("API_GATEWAY_OPERATOR_ROLE"),
		RateLimitPerSec: parseFloat(os.Getenv("API_GATEWAY_RATE_LIMIT_PER_SEC")),
		RateLimitBurst:  parseInt(os.Getenv("API_GATEWAY_RATE_LIMIT_BURST")),
		MaxInFlight:     parseInt(os.Getenv("API_GATEWAY_MAX_IN_FLIGHT")),
		QuotasFile:      os.Getenv("API_GATEWAY_QUOTAS_FILE"),
		SigningSecret:   signingSecret,
		NATSURL:         os.Getenv("API_GATEWAY_NATS_URL"),
		WealthAddr:      os.Getenv("API_GATEWAY_WEALTH_ADDR"),
		DataMasterAddr:  os.Getenv("API_GATEWAY_DATAMASTER_ADDR"),
		CopilotAddr:     os.Getenv("API_GATEWAY_COPILOT_ADDR"),
		TVSyncAddr:      os.Getenv("API_GATEWAY_TV_SYNC_ADDR"),
	}
	if err := cfg.validateAuth(); err != nil {
		return Config{}, err
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
	return nil
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
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
