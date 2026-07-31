package config

import (
	"strings"
	"testing"
)

func TestLoadDefaultsAndRequired(t *testing.T) {
	t.Setenv("KANZ_GATEWAY_URL", "https://api.eighred.com/")
	t.Setenv("KANZ_SSO_ISSUER", "https://login.eighred.com")
	t.Setenv("KANZ_CLIENT_ID", "")
	t.Setenv("KANZ_SCOPE", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GatewayURL != "https://api.eighred.com" { // trailing slash trimmed
		t.Errorf("GatewayURL = %q", cfg.GatewayURL)
	}
	if cfg.ClientID != "kanz-cli" || cfg.Scope != "openid profile" {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	t.Setenv("KANZ_GATEWAY_URL", "")
	t.Setenv("KANZ_SSO_ISSUER", "https://login.eighred.com")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when KANZ_GATEWAY_URL is unset")
	}

	t.Setenv("KANZ_GATEWAY_URL", "https://api.eighred.com")
	t.Setenv("KANZ_SSO_ISSUER", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when KANZ_SSO_ISSUER is unset")
	}
}

// EXACTLY ONE WAY TO HOLD A SESSION MAY BE CONFIGURED.
//
// Refusing both-at-once is the guard that makes a static bearer safe to have at
// all. A precedence rule ("SSO wins") reads as the careful choice and is the
// dangerous one: an operator who exports KANZ_TOKEN for a local experiment and
// forgets it keeps getting real SSO sessions until the day an issuer is briefly
// unset — and then silently switches to a stale bearer with no signal.
func TestIssuerAndDevTokenAreMutuallyExclusive(t *testing.T) {
	t.Setenv("KANZ_GATEWAY_URL", "https://gw.invalid")
	t.Setenv("KANZ_SSO_ISSUER", "https://sso.invalid")
	t.Setenv("KANZ_TOKEN", "pre-minted")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted both KANZ_SSO_ISSUER and KANZ_TOKEN — one would silently win, and " +
			"nobody would know which")
	}
	for _, want := range []string{"KANZ_SSO_ISSUER", "KANZ_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s — the operator cannot tell what to unset", err, want)
		}
	}
}

// Neither set is still refused, and the message now names both routes: the
// original said only "KANZ_SSO_ISSUER is required", which is a dead end for
// someone pointing kanz at a local gateway that has no SSO in front of it.
func TestNeitherIssuerNorDevTokenIsRefusedAndNamesBothRoutes(t *testing.T) {
	t.Setenv("KANZ_GATEWAY_URL", "https://gw.invalid")
	t.Setenv("KANZ_SSO_ISSUER", "")
	t.Setenv("KANZ_TOKEN", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted a configuration with no way to authenticate")
	}
	for _, want := range []string{"KANZ_SSO_ISSUER", "KANZ_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

// The dev path: a token and no issuer loads, and the token is carried through.
func TestDevTokenAloneIsAValidConfiguration(t *testing.T) {
	t.Setenv("KANZ_GATEWAY_URL", "https://gw.invalid")
	t.Setenv("KANZ_SSO_ISSUER", "")
	t.Setenv("KANZ_TOKEN", "  pre-minted  ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with only KANZ_TOKEN failed: %v", err)
	}
	if cfg.DevToken != "pre-minted" {
		t.Errorf("DevToken = %q, want it trimmed to %q — a trailing newline from a file or a shell "+
			"would otherwise be sent to the gateway inside the bearer", cfg.DevToken, "pre-minted")
	}
	if cfg.Issuer != "" {
		t.Errorf("Issuer = %q, want empty", cfg.Issuer)
	}
}
