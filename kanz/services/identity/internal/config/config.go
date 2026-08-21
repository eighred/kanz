// Package config loads the identity service's runtime configuration from the
// environment (#364).
//
// The identity service is where a person exchanges a credential for a token. It
// holds the signing key and the credential store, behind the gateway, so the
// gateway — which is the verifier — cannot forge what it verifies.
package config

import (
	"errors"
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eighred/kanz/pkg/secret"
)

// Config is the resolved identity configuration.
type Config struct {
	Listen   string
	LogLevel slog.Level

	// DatabaseURL is the credential store. It is deliberately UNSCOPED by tenant
	// — see identity.WhyNoTenantScope: login must find an account before it knows
	// which tenant the account belongs to.
	DatabaseURL string

	// SigningKeyFile is a PEM holding the P-256 EC private key tokens are signed
	// with. Empty requires AllowEphemeralKey.
	SigningKeyFile string
	// AllowEphemeralKey permits generating a throwaway key when none is
	// configured. Development only: every restart invalidates every session.
	AllowEphemeralKey bool

	// TokenIssuer and TokenAudience must match the api-gateway's configured
	// values EXACTLY. A mismatch is not a degraded mode — the gateway rejects
	// every token this service issues, so login succeeds and nothing else works.
	TokenIssuer   string
	TokenAudience string
	// TokenTTL bounds a session. Zero takes identity.DefaultTokenTTL.
	TokenTTL time.Duration

	// LoginBurst and LoginRefill tune the credential limiter. Zero takes the
	// package defaults, which are sized for a person signing in.
	LoginBurst  int
	LoginRefill time.Duration

	// OperatorRole enables AUTHENTICATED provisioning (#364) and names the role a
	// caller must hold to create an account.
	//
	// EMPTY DISABLES THE ROUTES ENTIRELY rather than defaulting to a role name.
	// Two reasons, and the second is the one that matters. Picking a default here
	// would decide who may create accounts on someone else's platform. And an
	// unconfigured deployment answering 403 instead of 404 tells a prober that a
	// provisioning surface exists to be attacked; not registering the route says
	// the truth, which is that it does not.
	//
	// Until this is set, cmd/kanz-invite (which needs the database credential)
	// remains the only path — correct for bootstrapping the first operator, and
	// not attributable to a person, which is why this exists.
	OperatorRole string

	// InviteTTL is how long a new invitation stays redeemable; zero uses the
	// domain default.
	InviteTTL time.Duration

	// OTLPEndpoint is the OTel collector for span export. Empty ⇒ none.
	OTLPEndpoint string
}

// Load reads the configuration, refusing anything that would start a service
// which appears healthy and cannot actually authenticate anyone.
func Load() (Config, error) {
	// RESOLVED BEFORE THE LITERAL, so a DECLARED-but-unreadable secret mount stops
	// Load here rather than resolving to "" — the same shape every other service's
	// Load uses, and the one this service was missing.
	//
	// IT WAS os.Getenv, AND THAT IS WHY THIS SERVICE COULD NOT START. The manifest
	// mounts IDENTITY_DATABASE_URL_FILE and sets no plaintext IDENTITY_DATABASE_URL,
	// so the read below returned "" and the check under it exited 2 — on a
	// distroless image with no shell and no command override, nothing resolved the
	// file on the process's behalf. The credential authority never came up, and
	// every login and every token in the estate waited on it.
	//
	// secret.Read prefers <K>_FILE and ERRORS on an unreadable mount instead of
	// falling through to the env var and then to "", which is the fall-through that
	// makes a failed CSI mount indistinguishable from a secret nobody configured.
	databaseURL, err := secret.Read("IDENTITY_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Listen:            env.Or("IDENTITY_LISTEN", ":8087"),
		LogLevel:          env.ParseLevelOr(os.Getenv("IDENTITY_LOG_LEVEL"), slog.LevelInfo),
		DatabaseURL:       databaseURL,
		SigningKeyFile:    os.Getenv("IDENTITY_SIGNING_KEY_FILE"),
		AllowEphemeralKey: os.Getenv("IDENTITY_ALLOW_EPHEMERAL_KEY") == "true",
		TokenIssuer:       os.Getenv("IDENTITY_TOKEN_ISSUER"),
		TokenAudience:     os.Getenv("IDENTITY_TOKEN_AUDIENCE"),
		TokenTTL:          parseDuration(os.Getenv("IDENTITY_TOKEN_TTL"), 0),
		OperatorRole:      strings.TrimSpace(os.Getenv("IDENTITY_OPERATOR_ROLE")),
		InviteTTL:         parseDuration(os.Getenv("IDENTITY_INVITE_TTL"), 0),
		LoginBurst:        parseInt(os.Getenv("IDENTITY_LOGIN_BURST"), 0),
		LoginRefill:       parseDuration(os.Getenv("IDENTITY_LOGIN_REFILL"), 0),
		OTLPEndpoint:      os.Getenv("IDENTITY_OTLP_ENDPOINT"),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("IDENTITY_DATABASE_URL is required — the credential store is " +
			"this service's entire state, and there is no in-memory mode that would be anything " +
			"other than an authentication service that forgets every account on restart")
	}
	// THE ISSUER AND AUDIENCE ARE A CONTRACT WITH THE GATEWAY, and a wrong value
	// fails in the least debuggable way available: login succeeds, a token comes
	// back, and every subsequent API call is a 401. Requiring them here turns that
	// into a refusal at startup with a name attached.
	if cfg.TokenIssuer == "" {
		return Config{}, errors.New("IDENTITY_TOKEN_ISSUER is required and must equal the gateway's " +
			"API_GATEWAY_OIDC_ISSUER exactly — a mismatch makes login succeed and every API call 401")
	}
	if cfg.TokenAudience == "" {
		return Config{}, errors.New("IDENTITY_TOKEN_AUDIENCE is required and must be an audience the " +
			"gateway accepts — a mismatch makes login succeed and every API call 401")
	}
	// Refuse the half-configuration rather than silently preferring one. An
	// operator who set both meant the file and would not discover that a
	// throwaway key was used instead until a restart dropped every session.
	if cfg.SigningKeyFile != "" && cfg.AllowEphemeralKey {
		return Config{}, errors.New("IDENTITY_SIGNING_KEY_FILE and IDENTITY_ALLOW_EPHEMERAL_KEY are " +
			"both set — these are alternatives, and silently preferring one would hide which key " +
			"is actually signing tokens")
	}
	return cfg, nil
}
func parseDuration(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(s)); err == nil && d > 0 {
		return d
	}
	return def
}

func parseInt(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return n
	}
	return def
}
