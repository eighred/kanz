// Package config loads the kanz terminal client's runtime configuration from
// the environment. The CLI is a thin front-of-house over the delivered edge:
// it needs only where the gateway is, and where the platform identity provider
// issues tokens.
package config

import (
	"errors"
	"os"
	"strings"
)

// Config is the resolved CLI configuration.
type Config struct {
	// GatewayURL is the base URL of the api-gateway edge (e.g.
	// https://api.eighred.com). The /v1 routes hang off it. Required.
	GatewayURL string
	// IdentityURL is the base URL of the platform identity provider that mints
	// this session's token (e.g. https://identity.eighred.com, or
	// http://localhost:8087 locally). Required to log in.
	//
	// IT REPLACED AN EXTERNAL SSO ISSUER, AND THAT IS THE POINT (#364). The CLI
	// used to authenticate through the Eighred SSO device flow — a service that
	// was never built — so the only way to hold a session was the pre-minted
	// DevToken below. The platform now issues its own tokens: this URL points at
	// services/identity, whose /login exchanges a subject and a credential for a
	// JWT the gateway already validates through the same OIDC path.
	IdentityURL string

	// DevToken is a pre-minted bearer used INSTEAD of signing in, from
	// KANZ_TOKEN. Empty in every normal deployment.
	//
	// WHY IT SURVIVED THE ARRIVAL OF A REAL SIGN-IN. Its original justification
	// is gone — it existed because "the SSO it replaces does not yet [ship]",
	// and the identity provider now does. What it is still for is narrower and
	// worth keeping: a gateway fronted by no identity service at all, which is
	// how the load stack and several CI steps run (they validate HS256 bearers
	// from kanz-devtoken and serve no OpenID discovery document). Those callers
	// have a token and nowhere to sign in; without this they could not drive the
	// client at all.
	//
	// It is honoured ONLY when IdentityURL is unset, and setting BOTH is refused
	// rather than resolved. See Load: a precedence rule is what would let this
	// quietly shadow a real sign-in, and the whole risk of a static-token path is
	// that nobody notices they are on it.
	//
	// kanz-monitor already accepts the same variable (--token / KANZ_TOKEN), so
	// this is the existing spelling rather than a new one.
	DevToken string
	// BookAccount is the broker account the Book pane shows positions and PnL
	// for; BookPortfolio is the portfolio it shows risk measures for (#84).
	//
	// They are SEPARATE ids because the two services key on different things —
	// tv-sync's projection by account, the risk engine by portfolio — and
	// assuming one string serves both is how one fund's risk ends up rendered
	// beside another fund's book. Empty is valid: the pane says which one is
	// unset rather than guessing a default and reading somebody else's book.
	BookAccount   string
	BookPortfolio string
	// SigningSecret is the shared HMAC key when the deployment sets
	// API_GATEWAY_SIGNING_SECRET on the gateway. Empty is valid — the gateway's
	// Signing middleware is a no-op when it holds no secret.
	//
	// Read HERE rather than by each surface that needs it. It was os.Getenv'd at
	// two separate call sites and missing entirely from a third (the Copilot
	// REPL), which is how that surface came to send unsigned requests and collect
	// 401s that read as an expired token (#198).
	SigningSecret string
}

// Load reads the configuration from KANZ_* environment variables, applying
// defaults, and validates that the required values are present.
func Load() (Config, error) {
	cfg := Config{
		GatewayURL:    strings.TrimRight(os.Getenv("KANZ_GATEWAY_URL"), "/"),
		IdentityURL:   strings.TrimRight(os.Getenv("KANZ_IDENTITY_URL"), "/"),
		DevToken:      strings.TrimSpace(os.Getenv("KANZ_TOKEN")),
		SigningSecret: os.Getenv("KANZ_SIGNING_SECRET"),
		BookAccount:   strings.TrimSpace(os.Getenv("KANZ_BOOK_ACCOUNT")),
		BookPortfolio: strings.TrimSpace(os.Getenv("KANZ_BOOK_PORTFOLIO")),
	}
	if cfg.GatewayURL == "" {
		return Config{}, errors.New("KANZ_GATEWAY_URL is required (the api-gateway base URL)")
	}
	// A RETIRED VARIABLE IS REFUSED, NOT IGNORED (#364).
	//
	// KANZ_SSO_ISSUER pointed at an external SSO that was never built. Silently
	// ignoring it would leave an operator whose environment still exports it
	// staring at "KANZ_IDENTITY_URL is required" while a variable that LOOKS like
	// the answer sits right there in their shell. Name it, and say what replaced
	// it. This check retires when nobody can still have it set — a deployment
	// question, not a code one.
	if os.Getenv("KANZ_SSO_ISSUER") != "" {
		return Config{}, errors.New("KANZ_SSO_ISSUER is set, and it no longer does anything: the CLI " +
			"signed in through an Eighred SSO device flow that was never built, and it now signs in " +
			"against the platform's own identity provider. Unset it and set KANZ_IDENTITY_URL to the " +
			"identity service's base URL instead")
	}
	// TWO WAYS TO HOLD A SESSION, AND EXACTLY ONE MAY BE CONFIGURED.
	//
	// Refusing both-at-once is the guard that makes the static token safe to
	// have at all. A precedence rule ("signing in wins") reads as the careful
	// choice and is the dangerous one: an operator who exports KANZ_TOKEN for a
	// local experiment and forgets it would keep getting real sessions until the
	// day the identity URL is briefly unset, and then silently switch to a stale
	// bearer with no signal that anything changed.
	//
	// Refusing is loud, immediate, and impossible to be on the wrong side of by
	// accident.
	switch {
	case cfg.IdentityURL != "" && cfg.DevToken != "":
		return Config{}, errors.New("KANZ_IDENTITY_URL and KANZ_TOKEN are both set, and they are two " +
			"different ways to hold a session: unset KANZ_TOKEN to sign in, or unset KANZ_IDENTITY_URL " +
			"to use the pre-minted token. This is refused rather than resolved so a leftover KANZ_TOKEN " +
			"can never quietly replace a real sign-in")
	case cfg.IdentityURL == "" && cfg.DevToken == "":
		return Config{}, errors.New("KANZ_IDENTITY_URL is required (the platform identity provider's " +
			"base URL, e.g. http://localhost:8087). For a gateway with no identity service in front of " +
			"it, set KANZ_TOKEN to a pre-minted bearer instead (e.g. from kanz-devtoken) and leave " +
			"KANZ_IDENTITY_URL unset")
	}
	return cfg, nil
}
