// Package config loads the kanz terminal client's runtime configuration from
// the environment. The CLI is a thin front-of-house over the delivered edge:
// it needs only where the gateway is, and where Eighred SSO issues tokens.
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
	// Issuer is the Eighred SSO issuer base URL the device flow authenticates
	// against (e.g. https://login.eighred.com). Required to log in.
	Issuer string
	// ClientID is the client identifier the CLI is registered as with Eighred
	// SSO (default "kanz-cli").
	ClientID string
	// Scope is the space-delimited scope requested at login (default
	// "openid profile").
	Scope string

	// DevToken is a pre-minted bearer used INSTEAD of signing in, from
	// KANZ_TOKEN. Empty in every normal deployment.
	//
	// IT EXISTS BECAUSE THE SSO IT REPLACES DOES NOT YET. kanz can only
	// authenticate through the Eighred SSO device flow, so until that service
	// ships there is no way to drive the client against a local gateway — the
	// load stack validates HS256 JWTs and serves no OpenID discovery document,
	// so /login can only 404 there.
	//
	// It is honoured ONLY when Issuer is unset, and setting BOTH is refused
	// rather than resolved. See Load: a precedence rule is what would let this
	// quietly shadow a real SSO session, and the whole risk of a static-token
	// path is that nobody notices they are on it.
	//
	// kanz-monitor already accepts the same variable (--token / KANZ_TOKEN), so
	// this is the existing spelling rather than a new one.
	DevToken string
}

// Load reads the configuration from KANZ_* environment variables, applying
// defaults, and validates that the required values are present.
func Load() (Config, error) {
	cfg := Config{
		GatewayURL: strings.TrimRight(os.Getenv("KANZ_GATEWAY_URL"), "/"),
		Issuer:     strings.TrimRight(os.Getenv("KANZ_SSO_ISSUER"), "/"),
		ClientID:   os.Getenv("KANZ_CLIENT_ID"),
		Scope:      os.Getenv("KANZ_SCOPE"),
		DevToken:   strings.TrimSpace(os.Getenv("KANZ_TOKEN")),
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "kanz-cli"
	}
	if cfg.Scope == "" {
		cfg.Scope = "openid profile"
	}
	if cfg.GatewayURL == "" {
		return Config{}, errors.New("KANZ_GATEWAY_URL is required (the api-gateway base URL)")
	}
	// TWO WAYS TO HOLD A SESSION, AND EXACTLY ONE MAY BE CONFIGURED.
	//
	// Refusing both-at-once is the guard that makes the static token safe to
	// have at all. A precedence rule ("SSO wins") reads as the careful choice
	// and is the dangerous one: an operator who exports KANZ_TOKEN for a local
	// experiment and forgets it would keep getting real SSO sessions until the
	// day an issuer is briefly unset, and then silently switch to a stale bearer
	// with no signal that anything changed.
	//
	// Refusing is loud, immediate, and impossible to be on the wrong side of by
	// accident.
	switch {
	case cfg.Issuer != "" && cfg.DevToken != "":
		return Config{}, errors.New("KANZ_SSO_ISSUER and KANZ_TOKEN are both set, and they are two " +
			"different ways to hold a session: unset KANZ_TOKEN to sign in through SSO, or unset " +
			"KANZ_SSO_ISSUER to use the pre-minted token. This is refused rather than resolved so a " +
			"leftover KANZ_TOKEN can never quietly replace a real sign-in")
	case cfg.Issuer == "" && cfg.DevToken == "":
		return Config{}, errors.New("KANZ_SSO_ISSUER is required (the Eighred SSO issuer URL). " +
			"For a local gateway that has no SSO in front of it, set KANZ_TOKEN to a pre-minted " +
			"bearer instead (e.g. from kanz-devtoken) and leave KANZ_SSO_ISSUER unset")
	}
	return cfg, nil
}
