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
		"API_GATEWAY_ALLOW_DEV_HS256",
		"API_GATEWAY_REQUIRED_ROLE",
		"API_GATEWAY_TRADE_ROLE",
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
	t.Setenv("API_GATEWAY_TRADE_ROLE", "kanz-trader")

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
		t.Setenv("API_GATEWAY_TRADE_ROLE", "kanz-trader")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() = %v; want a configured gateway to start", err)
		}
		if cfg.OIDCIssuer != "https://login.eighred.com" || cfg.RequiredRole != "kanz-user" || cfg.TradeRole != "kanz-trader" {
			t.Errorf("cfg = %+v", cfg)
		}
	})

	t.Run("dev hs256 secret with the explicit opt-in", func(t *testing.T) {
		clearAuthEnv(t)
		t.Setenv("API_GATEWAY_JWT_SECRET", "dev-secret")
		t.Setenv("API_GATEWAY_ALLOW_DEV_HS256", "true")
		t.Setenv("API_GATEWAY_REQUIRED_ROLE", "kanz-user")
		t.Setenv("API_GATEWAY_TRADE_ROLE", "kanz-trader")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() = %v; want a configured gateway to start", err)
		}
		if cfg.JWTSecret != "dev-secret" || !cfg.AllowDevHS256 {
			t.Errorf("cfg = %+v", cfg)
		}
	})
}

// THE DEV CREDENTIAL MUST NOT BE REACHABLE BY OMISSION (#242).
//
// API_GATEWAY_JWT_SECRET alone used to be a complete and silent authentication
// configuration. That is how a staging or DR gateway comes up on a symmetric
// HMAC key: not because anyone chose it, but because a partial copy of the
// production environment dropped the OIDC issuer and kept the secret, and
// nothing in the config could tell the two apart.
//
// Gating on a SEPARATE, purpose-named boolean rather than on the secret is the
// point. A secret arrives by inheritance — a copied ConfigMap, a Vault path that
// still resolves; a variable named ALLOW_DEV_HS256 has to be typed by somebody
// who read what it turns on.
func TestLoadRefusesDevHS256WithoutTheOptIn(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("API_GATEWAY_JWT_SECRET", "dev-secret")
	t.Setenv("API_GATEWAY_REQUIRED_ROLE", "kanz-user")
	t.Setenv("API_GATEWAY_TRADE_ROLE", "kanz-trader")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil error with a JWT secret and no API_GATEWAY_ALLOW_DEV_HS256 — " +
			"the dev HS256 credential is reachable by omission, which is the #242 defect")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_ALLOW_DEV_HS256") {
		t.Errorf("error does not name API_GATEWAY_ALLOW_DEV_HS256 (the operator reading the crash "+
			"must know what to set): %v", err)
	}
}

// The opt-in is a real boolean, not a presence check: setting it to false is a
// deliberate NO and must refuse exactly as omitting it does. A presence check
// would turn `API_GATEWAY_ALLOW_DEV_HS256=false` — which is what an operator
// writes to turn something OFF — into an enable.
func TestLoadRefusesDevHS256WhenTheOptInIsFalse(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("API_GATEWAY_JWT_SECRET", "dev-secret")
	t.Setenv("API_GATEWAY_ALLOW_DEV_HS256", "false")
	t.Setenv("API_GATEWAY_REQUIRED_ROLE", "kanz-user")
	t.Setenv("API_GATEWAY_TRADE_ROLE", "kanz-trader")

	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted the HS256 arm with API_GATEWAY_ALLOW_DEV_HS256=false")
	}
}

// An unreadable value is an ERROR, not a silent false. An operator who wrote
// `=yes` stated an intent as clearly as one who wrote `=true`; answering that
// with "set API_GATEWAY_ALLOW_DEV_HS256" — a variable they can see they already
// set — is the message that gets the check deleted rather than the value fixed.
func TestLoadRefusesAnUnparseableOptIn(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("API_GATEWAY_JWT_SECRET", "dev-secret")
	t.Setenv("API_GATEWAY_ALLOW_DEV_HS256", "yes")
	t.Setenv("API_GATEWAY_REQUIRED_ROLE", "kanz-user")
	t.Setenv("API_GATEWAY_TRADE_ROLE", "kanz-trader")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted API_GATEWAY_ALLOW_DEV_HS256=yes")
	}
	if !strings.Contains(err.Error(), "not a boolean") {
		t.Errorf("error does not say the value is unreadable, so the operator will re-read the "+
			"variable name instead of the value: %v", err)
	}
}

// The opt-in gates the HS256 ARM, not the gateway. With OIDC configured the
// HS256 validator is never constructed (main.go's switch takes the OIDC arm
// first), so demanding the flag there would be config for a code path that
// cannot run — and this repository's rule is that a control must describe
// something real or it gets routed around.
func TestLoadDoesNotRequireTheOptInWhenOIDCIsConfigured(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("API_GATEWAY_OIDC_ISSUER", "https://login.eighred.com")
	t.Setenv("API_GATEWAY_JWT_SECRET", "left-over-dev-secret")
	t.Setenv("API_GATEWAY_REQUIRED_ROLE", "kanz-user")
	t.Setenv("API_GATEWAY_TRADE_ROLE", "kanz-trader")

	if _, err := Load(); err != nil {
		t.Fatalf("Load() = %v; an OIDC gateway that still carries a stale JWT secret must start — "+
			"the HS256 arm is unreachable, so there is nothing to opt into", err)
	}
}

// TestLoadRefusesWithoutATradeRole (SEC-M2): one role for everything meant any principal who
// could READ could TRADE — the token handed to an analyst to look at exposure would submit an
// order to a live exchange. The gateway must be told who may move capital, and it will not
// guess.
//
// It could have failed closed instead (no trade role ⇒ nobody trades). That is a TRADING
// OUTAGE DRESSED AS A CONTROL: it would be discovered by an order that quietly did not go
// out. Refusing to start is discovered by whoever deployed it, immediately.
func TestLoadRefusesWithoutATradeRole(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("API_GATEWAY_OIDC_ISSUER", "https://login.eighred.com")
	t.Setenv("API_GATEWAY_REQUIRED_ROLE", "kanz-user")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil error with no trade role; want refusal — one role for everything means every reader can trade")
	}
	if !strings.Contains(err.Error(), "API_GATEWAY_TRADE_ROLE") {
		t.Errorf("error does not name API_GATEWAY_TRADE_ROLE (the operator reading the crash must know what to set): %v", err)
	}
}

// TestLoadRefusesATradeRoleThatIsTheBaselineRole is SEC-M2 undone in one line of config.
//
// EVERY authenticated caller carries the baseline role — it is what admits them to the
// gateway at all (SEC-M1). Setting it as the trade role hands order entry to everyone who can
// read, which is the exact failure this task exists to end, restored by a config edit that
// looks harmless in a diff.
func TestLoadRefusesATradeRoleThatIsTheBaselineRole(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("API_GATEWAY_OIDC_ISSUER", "https://login.eighred.com")
	t.Setenv("API_GATEWAY_REQUIRED_ROLE", "kanz-user")
	t.Setenv("API_GATEWAY_TRADE_ROLE", "kanz-user")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted the baseline role AS the trade role — every reader can now submit orders")
	}
}
