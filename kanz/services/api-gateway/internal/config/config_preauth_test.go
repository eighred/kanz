package config

import (
	"strings"
	"testing"
)

// devAuthEnv is the minimum that gets Load past validateAuth, so the cases below
// are testing the trusted-proxy pairing and nothing else.
func devAuthEnv(t *testing.T) {
	t.Helper()
	clearAuthEnv(t)
	t.Setenv("API_GATEWAY_JWT_SECRET", "dev-secret")
	t.Setenv("API_GATEWAY_ALLOW_DEV_HS256", "true")
	t.Setenv("API_GATEWAY_REQUIRED_ROLE", "kanz-user")
	t.Setenv("API_GATEWAY_TRADE_ROLE", "kanz-trader")
	t.Setenv("API_GATEWAY_TRUSTED_PROXY_HEADER", "")
	t.Setenv("API_GATEWAY_TRUSTED_PROXIES", "")
}

// A HALF-CONFIGURED PROXY TRUST IS REFUSED, IN BOTH DIRECTIONS (#835).
//
// The pre-auth limiter keys on the caller's address. Naming a header with nobody
// trusted to send it — or trusting peers without naming a header — reads as
// configured and honours nothing: the operator believes the bound is per-caller
// while it is per-edge, which behind an ingress controller is one bucket for
// everyone. Refusing to start is the only way the two states do not look alike.
func TestLoadRefusesAHalfConfiguredProxyTrust(t *testing.T) {
	cases := map[string]struct{ header, proxies string }{
		"header without trusted peers": {header: "X-Forwarded-For"},
		"trusted peers without header": {proxies: "10.0.0.0/8"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			devAuthEnv(t)
			t.Setenv("API_GATEWAY_TRUSTED_PROXY_HEADER", tc.header)
			t.Setenv("API_GATEWAY_TRUSTED_PROXIES", tc.proxies)

			_, err := Load()
			if err == nil {
				t.Fatal("Load() = nil error for a half-configured trusted proxy; want refusal")
			}
			for _, want := range []string{"API_GATEWAY_TRUSTED_PROXY_HEADER", "API_GATEWAY_TRUSTED_PROXIES"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not name %s — the operator reading the crash must know "+
						"which pair to complete: %v", want, err)
				}
			}
		})
	}
}

// NEITHER IS THE SUPPORTED DEFAULT, and it must not be a refusal. Direct local
// development has no edge in front of the gateway, and the peer address is then
// the right answer — too strict rather than absent.
func TestLoadAcceptsNoProxyTrustAtAll(t *testing.T) {
	devAuthEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with no trusted proxy configured: %v", err)
	}
	if cfg.TrustedProxyHeader != "" || len(cfg.TrustedProxies) != 0 {
		t.Errorf("TrustedProxyHeader=%q TrustedProxies=%v, want both empty",
			cfg.TrustedProxyHeader, cfg.TrustedProxies)
	}
}

// BOTH SET IS READ THROUGH, including the list split — a guard that only proved
// the refusal would pass with the fields never populated at all.
func TestLoadReadsBothHalvesOfTheProxyTrust(t *testing.T) {
	devAuthEnv(t)
	t.Setenv("API_GATEWAY_TRUSTED_PROXY_HEADER", "X-Forwarded-For")
	t.Setenv("API_GATEWAY_TRUSTED_PROXIES", "10.0.0.0/8,192.0.2.7")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.TrustedProxyHeader != "X-Forwarded-For" {
		t.Errorf("TrustedProxyHeader = %q, want X-Forwarded-For", cfg.TrustedProxyHeader)
	}
	if len(cfg.TrustedProxies) != 2 ||
		cfg.TrustedProxies[0] != "10.0.0.0/8" || cfg.TrustedProxies[1] != "192.0.2.7" {
		t.Errorf("TrustedProxies = %v, want [10.0.0.0/8 192.0.2.7]", cfg.TrustedProxies)
	}
}
