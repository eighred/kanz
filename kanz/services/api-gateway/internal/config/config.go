package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
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

	// JWTSecret is the HS256 shared secret the bundled minimal JWT validator
	// verifies bearer tokens against (API-01d). Empty ⇒ authentication is
	// DISABLED (local/dev only). AUTH-01a replaces this with real OIDC/JWKS.
	JWTSecret string
	// RequiredRole, when set, is the role a Principal must carry to reach any
	// query endpoint (deny-by-default once auth is enabled).
	RequiredRole string

	// RateLimitPerSec / RateLimitBurst configure the per-tenant token bucket
	// (API-01d). A non-positive rate disables rate limiting.
	RateLimitPerSec float64
	RateLimitBurst  int

	// SigningSecret, when set, requires every request to carry a valid HMAC
	// X-Signature over method+path+body (API-01d request signing). Empty ⇒
	// signing is not enforced.
	SigningSecret string
}

func Load() (Config, error) {
	return Config{
		Listen:          envOr("API_GATEWAY_LISTEN", ":8080"),
		LogLevel:        parseLevel(envOr("API_GATEWAY_LOG_LEVEL", "info")),
		Source:          envOr("API_GATEWAY_SOURCE", "api-gateway"),
		OTLPEndpoint:    os.Getenv("API_GATEWAY_OTLP_ENDPOINT"),
		RiskEngineAddr:  os.Getenv("API_GATEWAY_RISK_ENGINE_ADDR"),
		SPIFFESocket:    os.Getenv("API_GATEWAY_SPIFFE_SOCKET"),
		JWTSecret:       secret("API_GATEWAY_JWT_SECRET"),
		RequiredRole:    os.Getenv("API_GATEWAY_REQUIRED_ROLE"),
		RateLimitPerSec: parseFloat(os.Getenv("API_GATEWAY_RATE_LIMIT_PER_SEC")),
		RateLimitBurst:  parseInt(os.Getenv("API_GATEWAY_RATE_LIMIT_BURST")),
		SigningSecret:   secret("API_GATEWAY_SIGNING_SECRET"),
	}, nil
}

// secret prefers a CSI/Vault file mount (<k>_FILE) over a plaintext <k> env
// var (SEC-01d), so secrets are never plaintext in the pod spec.
func secret(k string) string {
	if p := os.Getenv(k + "_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv(k)
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
