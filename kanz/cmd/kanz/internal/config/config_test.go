package config

import (
	"strings"
	"testing"
)

func TestLoadTrimsAndCarriesTheIdentityURL(t *testing.T) {
	t.Setenv("KANZ_GATEWAY_URL", "https://api.eighred.com/")
	t.Setenv("KANZ_IDENTITY_URL", "https://identity.eighred.com/")
	t.Setenv("KANZ_TOKEN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GatewayURL != "https://api.eighred.com" { // trailing slash trimmed
		t.Errorf("GatewayURL = %q", cfg.GatewayURL)
	}
	// TRIMMED FOR THE SAME REASON THE GATEWAY URL IS: every path is appended to
	// this base, so a trailing slash turns /login into //login. Some muxes route
	// that and some 404 it, and the failure would read as "the identity service
	// is down".
	if cfg.IdentityURL != "https://identity.eighred.com" {
		t.Errorf("IdentityURL = %q, want the trailing slash trimmed", cfg.IdentityURL)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	t.Setenv("KANZ_GATEWAY_URL", "")
	t.Setenv("KANZ_IDENTITY_URL", "https://identity.eighred.com")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when KANZ_GATEWAY_URL is unset")
	}

	t.Setenv("KANZ_GATEWAY_URL", "https://api.eighred.com")
	t.Setenv("KANZ_IDENTITY_URL", "")
	t.Setenv("KANZ_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when neither KANZ_IDENTITY_URL nor KANZ_TOKEN is set")
	}
}

// EXACTLY ONE WAY TO HOLD A SESSION MAY BE CONFIGURED.
//
// Refusing both-at-once is the guard that makes a static bearer safe to have at
// all. A precedence rule ("signing in wins") reads as the careful choice and is
// the dangerous one: an operator who exports KANZ_TOKEN for a local experiment
// and forgets it keeps getting real sessions until the day the identity URL is
// briefly unset — and then silently switches to a stale bearer with no signal.
func TestIdentityURLAndDevTokenAreMutuallyExclusive(t *testing.T) {
	t.Setenv("KANZ_GATEWAY_URL", "https://gw.invalid")
	t.Setenv("KANZ_IDENTITY_URL", "https://identity.invalid")
	t.Setenv("KANZ_TOKEN", "pre-minted")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted both KANZ_IDENTITY_URL and KANZ_TOKEN — one would silently win, and " +
			"nobody would know which")
	}
	for _, want := range []string{"KANZ_IDENTITY_URL", "KANZ_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s — the operator cannot tell what to unset", err, want)
		}
	}
}

// Neither set is still refused, and the message names both routes: saying only
// "KANZ_IDENTITY_URL is required" is a dead end for someone pointing kanz at a
// local gateway that has no identity service in front of it.
func TestNeitherIdentityURLNorDevTokenIsRefusedAndNamesBothRoutes(t *testing.T) {
	t.Setenv("KANZ_GATEWAY_URL", "https://gw.invalid")
	t.Setenv("KANZ_IDENTITY_URL", "")
	t.Setenv("KANZ_TOKEN", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted a configuration with no way to authenticate")
	}
	for _, want := range []string{"KANZ_IDENTITY_URL", "KANZ_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

// The dev path: a token and no identity URL loads, and the token is carried
// through.
func TestDevTokenAloneIsAValidConfiguration(t *testing.T) {
	t.Setenv("KANZ_GATEWAY_URL", "https://gw.invalid")
	t.Setenv("KANZ_IDENTITY_URL", "")
	t.Setenv("KANZ_TOKEN", "  pre-minted  ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with only KANZ_TOKEN failed: %v", err)
	}
	if cfg.DevToken != "pre-minted" {
		t.Errorf("DevToken = %q, want it trimmed to %q — a trailing newline from a file or a shell "+
			"would otherwise be sent to the gateway inside the bearer", cfg.DevToken, "pre-minted")
	}
	if cfg.IdentityURL != "" {
		t.Errorf("IdentityURL = %q, want empty", cfg.IdentityURL)
	}
}

// A RETIRED VARIABLE IS REFUSED, NOT IGNORED (#364).
//
// KANZ_SSO_ISSUER pointed at an Eighred SSO device flow that was never built.
// The danger is not that it stops working — it never worked — but that an
// operator whose shell still exports it reads "KANZ_IDENTITY_URL is required"
// while a variable that LOOKS like the answer sits right there. Silence would
// cost them the one thing the error is for.
func TestTheRetiredSSOIssuerIsRefusedByName(t *testing.T) {
	t.Setenv("KANZ_GATEWAY_URL", "https://gw.invalid")
	t.Setenv("KANZ_IDENTITY_URL", "https://identity.invalid")
	t.Setenv("KANZ_TOKEN", "")
	t.Setenv("KANZ_SSO_ISSUER", "https://login.eighred.com")

	_, err := Load()
	if err == nil {
		t.Fatal("Load ignored KANZ_SSO_ISSUER. It is retired, and an operator who still exports it " +
			"must be told so rather than left to wonder why their issuer has no effect")
	}
	for _, want := range []string{"KANZ_SSO_ISSUER", "KANZ_IDENTITY_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s — it must say what is retired AND what replaced it", err, want)
		}
	}
}
