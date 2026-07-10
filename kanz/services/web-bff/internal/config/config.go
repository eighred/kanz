// Package config loads the web-BFF's runtime configuration from the
// environment. The BFF is the browser front-of-house over the delivered /v1
// edge (PS-02a): it runs the OIDC authorization-code + PKCE login against
// Eighred SSO, holds the issued token server-side in a session, and
// reverse-proxies the browser's API calls to the api-gateway with that token
// attached. It adds no backend analytics — it is the CLI's browser counterpart.
package config

import (
	"errors"
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

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string
}

// Load reads the configuration from WEB_BFF_* environment variables with
// production-safe defaults, and validates the required values.
func Load() (Config, error) {
	cfg := Config{
		Listen:        envOr("WEB_BFF_LISTEN", ":8084"),
		LogLevel:      parseLevel(os.Getenv("WEB_BFF_LOG_LEVEL")),
		Issuer:        strings.TrimRight(os.Getenv("WEB_BFF_SSO_ISSUER"), "/"),
		ClientID:      envOr("WEB_BFF_CLIENT_ID", "kanz-web"),
		RedirectURL:   os.Getenv("WEB_BFF_REDIRECT_URL"),
		Scope:         envOr("WEB_BFF_SCOPE", "openid profile"),
		GatewayURL:    strings.TrimRight(os.Getenv("WEB_BFF_GATEWAY_URL"), "/"),
		SessionTTL:    parseDuration(os.Getenv("WEB_BFF_SESSION_TTL"), time.Hour),
		SecureCookies: os.Getenv("WEB_BFF_INSECURE_COOKIES") == "",
		OTLPEndpoint:  os.Getenv("WEB_BFF_OTLP_ENDPOINT"),
	}
	if cfg.Issuer == "" {
		return Config{}, errors.New("WEB_BFF_SSO_ISSUER is required (the Eighred SSO issuer URL)")
	}
	if cfg.RedirectURL == "" {
		return Config{}, errors.New("WEB_BFF_REDIRECT_URL is required (this BFF's /auth/callback URL)")
	}
	if cfg.GatewayURL == "" {
		return Config{}, errors.New("WEB_BFF_GATEWAY_URL is required (the api-gateway base URL)")
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
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

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
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
