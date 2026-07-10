package config

import "testing"

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
