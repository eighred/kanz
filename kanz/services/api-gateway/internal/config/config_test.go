package config

import (
	"strings"
	"testing"
)

// clearAuthEnv isolates each case from the developer's own environment: every
// variable Load reads for the authentication decision is explicitly emptied,
// then the case sets back only what it means to test.
func clearAuthEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"API_GATEWAY_OIDC_ISSUER",
		"API_GATEWAY_JWT_SECRET",
		"API_GATEWAY_JWT_SECRET_FILE",
		"API_GATEWAY_REQUIRED_ROLE",
	} {
		t.Setenv(k, "")
	}
}

// TestLoadRefusesUnauthenticated: the gateway is the platform's sole identity
// authority, and with no OIDC issuer and no JWT secret it used to log a WARN
// and then serve /v1/* — including POST /v1/orders — to anyone. Nobody has said
// how to authenticate a caller, so it must not start.
func TestLoadRefusesUnauthenticated(t *testing.T) {
	clearAuthEnv(t)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil error with no OIDC issuer and no JWT secret; want refusal")
	}
	for _, want := range []string{"API_GATEWAY_OIDC_ISSUER", "API_GATEWAY_JWT_SECRET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s (the operator reading the crash must know what to set): %v", want, err)
		}
	}
}

// TestLoadRefusesWithoutRequiredRole: authentication without authorization means
// every token the issuer ever minted — for any client, any purpose — reaches
// every /v1 route, POST /v1/orders included.
func TestLoadRefusesWithoutRequiredRole(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("API_GATEWAY_OIDC_ISSUER", "https://login.eighred.com")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil error with an issuer but no required role; want refusal")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_REQUIRED_ROLE") {
		t.Errorf("error does not name API_GATEWAY_REQUIRED_ROLE: %v", err)
	}
}

// TestLoadAcceptsConfiguredAuth: the two shapes that authenticate a caller —
// production OIDC and the dev HS256 secret — both start.
func TestLoadAcceptsConfiguredAuth(t *testing.T) {
	t.Run("oidc issuer", func(t *testing.T) {
		clearAuthEnv(t)
		t.Setenv("API_GATEWAY_OIDC_ISSUER", "https://login.eighred.com")
		t.Setenv("API_GATEWAY_REQUIRED_ROLE", "kanz-user")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() = %v; want a configured gateway to start", err)
		}
		if cfg.OIDCIssuer != "https://login.eighred.com" || cfg.RequiredRole != "kanz-user" {
			t.Errorf("cfg = %+v", cfg)
		}
	})

	t.Run("dev hs256 secret", func(t *testing.T) {
		clearAuthEnv(t)
		t.Setenv("API_GATEWAY_JWT_SECRET", "dev-secret")
		t.Setenv("API_GATEWAY_REQUIRED_ROLE", "kanz-user")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() = %v; want a configured gateway to start", err)
		}
		if cfg.JWTSecret != "dev-secret" {
			t.Errorf("cfg = %+v", cfg)
		}
	})
}
