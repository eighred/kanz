// Package config loads the web-BFF's runtime configuration from the
// environment. The BFF is the browser front-of-house over the delivered /v1
// edge (PS-02a): it runs the OIDC authorization-code + PKCE login against
// Eighred SSO, holds the issued token server-side in a session, and
// reverse-proxies the browser's API calls to the api-gateway with that token
// attached. It adds no backend analytics — it is the CLI's browser counterpart.
package config

import (
	"errors"
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Config is the resolved BFF configuration.
type Config struct {
	Listen   string
	LogLevel slog.Level

	// Issuer is the Eighred SSO issuer base URL the login flow runs against.
	// Endpoints are discovered from {Issuer}/.well-known/openid-configuration.
	Issuer string
	// ClientID is the identifier the BFF is registered as with Eighred SSO. The
	// SSO client registry must allow RedirectURL as a redirect_uri for it.
	ClientID string
	// RedirectURL is this BFF's public callback URL (e.g.
	// https://app.eighred.com/auth/callback) the SSO redirects the code back to.
	RedirectURL string
	// Scope is the space-delimited scope requested at login.
	Scope string

	// GatewayURL is the base URL of the api-gateway edge the BFF proxies to.
	GatewayURL string

	// SessionTTL bounds how long a browser session (and its held token) lives
	// before re-login is required.
	SessionTTL time.Duration
	// SecureCookies sets the Secure flag on cookies. Default true; set
	// WEB_BFF_INSECURE_COOKIES=1 for local http development only.
	SecureCookies bool

	// IdentityURL is the platform identity provider (#364) the credential login
	// exchanges against. REQUIRED — it is how anyone signs in.
	IdentityURL string

	// TrustedProxyHeader names the forwarded-for header the edge sets, e.g.
	// "CF-Connecting-IP" under the Cloudflare Tunnel model. Empty ⇒ the peer
	// address is used and no header is honoured.
	TrustedProxyHeader string
	// TrustedProxies are the peers permitted to set that header.
	//
	// BOTH ARE REQUIRED TOGETHER OR THE HEADER IS IGNORED, and that is the safe
	// direction: the login limiter keys on the resolved address, so honouring a
	// header from an untrusted peer turns the limiter into nothing — an attacker
	// varies the value per request and guesses credentials unbounded, while the
	// traffic looks like many well-behaved clients. Ignoring it merely makes the
	// limiter too strict.
	TrustedProxies []string

	// StaticDir is the compiled SPA (kanz-web/dist) this BFF serves on its OWN
	// origin, so the httpOnly session cookie needs no CORS and no cookie-domain
	// syncing. Empty ⇒ API only, the local shape where Vite serves the SPA and
	// proxies /api and /auth back here.
	StaticDir string

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string
}

// Load reads the configuration from WEB_BFF_* environment variables with
// production-safe defaults, and validates the required values.
func Load() (Config, error) {
	cfg := Config{
		Listen:        env.Or("WEB_BFF_LISTEN", ":8084"),
		LogLevel:      env.ParseLevelOr(os.Getenv("WEB_BFF_LOG_LEVEL"), slog.LevelInfo),
		Issuer:        strings.TrimRight(os.Getenv("WEB_BFF_SSO_ISSUER"), "/"),
		ClientID:      env.Or("WEB_BFF_CLIENT_ID", "kanz-web"),
		RedirectURL:   os.Getenv("WEB_BFF_REDIRECT_URL"),
		Scope:         env.Or("WEB_BFF_SCOPE", "openid profile"),
		GatewayURL:    strings.TrimRight(os.Getenv("WEB_BFF_GATEWAY_URL"), "/"),
		SessionTTL:    parseDuration(os.Getenv("WEB_BFF_SESSION_TTL"), time.Hour),
		SecureCookies: os.Getenv("WEB_BFF_INSECURE_COOKIES") == "",
		OTLPEndpoint:  os.Getenv("WEB_BFF_OTLP_ENDPOINT"),

		IdentityURL:        strings.TrimRight(os.Getenv("WEB_BFF_IDENTITY_URL"), "/"),
		TrustedProxyHeader: os.Getenv("WEB_BFF_TRUSTED_PROXY_HEADER"),
		TrustedProxies:     env.SplitList(os.Getenv("WEB_BFF_TRUSTED_PROXIES")),
		StaticDir:          os.Getenv("WEB_BFF_STATIC_DIR"),
	}
	if cfg.IdentityURL == "" {
		return Config{}, errors.New("WEB_BFF_IDENTITY_URL is required (the identity service base URL) — " +
			"it is how anyone signs in")
	}
	if cfg.GatewayURL == "" {
		return Config{}, errors.New("WEB_BFF_GATEWAY_URL is required (the api-gateway base URL)")
	}
	// SSO IS NOW OPTIONAL, AND THAT IS THE POINT OF THIS CHANGE. WEB_BFF_SSO_ISSUER
	// used to be required, so this service could not START without an Eighred SSO
	// that does not exist — the browser path was as dead as the TUI's /login.
	// Credential login against the identity service replaces it.
	//
	// The OIDC arm is kept rather than deleted, for the same reason pkg/auth keeps
	// its authenticator seam: a fund manager who requires their own Okta or Entra
	// tenant is then configuration, not a re-architecture. Configured HALF-way is
	// the one thing refused — an issuer with no redirect URL would advertise a
	// login route that cannot complete.
	if (cfg.Issuer == "") != (cfg.RedirectURL == "") {
		return Config{}, errors.New("WEB_BFF_SSO_ISSUER and WEB_BFF_REDIRECT_URL must be set together " +
			"or not at all — an issuer with no redirect URL offers a login that cannot complete")
	}
	// Naming a header with nobody trusted to send it, or the reverse, silently
	// disables it. Refuse rather than run in a state the operator believes is
	// configured — see clientip's package comment for what that costs.
	if (cfg.TrustedProxyHeader == "") != (len(cfg.TrustedProxies) == 0) {
		return Config{}, errors.New("WEB_BFF_TRUSTED_PROXY_HEADER and WEB_BFF_TRUSTED_PROXIES must be " +
			"set together or not at all — one without the other reads as configured and honours nothing")
	}
	return cfg, nil
}
func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
