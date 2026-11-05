package config

import (
	"os"
	"strings"
	"testing"
)

func TestRustBondShadowEnvironment(t *testing.T) {
	const p = "RISK_ENGINE_RUST_BOND_SHADOW_"
	for _, k := range []string{"ENABLED", "EXECUTABLE", "EXPECTED_VERSION", "TIMEOUT"} {
		key := p + k
		old, exists := os.LookupEnv(key)
		if e := os.Unsetenv(key); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			if exists {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	if c, e := rustBondShadow(); e != nil || c.Enabled {
		t.Fatal("default off", e)
	}
	for _, tc := range []struct{ k, v string }{{"ENABLED", "typo"}, {"EXECUTABLE", "relative"}, {"EXPECTED_VERSION", "stamp"}, {"TIMEOUT", "bad"}, {"TIMEOUT", "1s"}} {
		t.Run(tc.k+tc.v, func(t *testing.T) {
			t.Setenv(p+tc.k, tc.v)
			if _, e := rustBondShadow(); e == nil {
				t.Fatal("partial or malformed configuration accepted")
			}
		})
	}
}

func TestRustBondShadowRequiresResolvedTermsDatabase(t *testing.T) {
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	for k, v := range map[string]string{"RISK_ENGINE_RUST_BOND_SHADOW_ENABLED": "true", "RISK_ENGINE_RUST_BOND_SHADOW_EXECUTABLE": exe, "RISK_ENGINE_RUST_BOND_SHADOW_EXPECTED_VERSION": "stamp", "RISK_ENGINE_RUST_BOND_SHADOW_TIMEOUT": "1s", "RISK_ENGINE_CALIBRATION_INTERVAL": "1m", "RISK_ENGINE_CALIBRATION_RATES": "USD", "RISK_ENGINE_NATS_URL": "nats://fixture.invalid", "RISK_ENGINE_MARKETDATA_DATABASE_URL": "", "RISK_ENGINE_MARKETDATA_DATABASE_URL_FILE": ""} {
		t.Setenv(k, v)
	}
	if _, e := Load(); e == nil || !strings.Contains(e.Error(), "market-data database") {
		t.Fatalf("expected resolved terms database refusal, got %v", e)
	}
}
