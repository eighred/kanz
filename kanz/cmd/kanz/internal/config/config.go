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
}

// Load reads the configuration from KANZ_* environment variables, applying
// defaults, and validates that the required values are present.
func Load() (Config, error) {
	cfg := Config{
		GatewayURL: strings.TrimRight(os.Getenv("KANZ_GATEWAY_URL"), "/"),
		Issuer:     strings.TrimRight(os.Getenv("KANZ_SSO_ISSUER"), "/"),
		ClientID:   os.Getenv("KANZ_CLIENT_ID"),
		Scope:      os.Getenv("KANZ_SCOPE"),
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
	if cfg.Issuer == "" {
		return Config{}, errors.New("KANZ_SSO_ISSUER is required (the Eighred SSO issuer URL)")
	}
	return cfg, nil
}
