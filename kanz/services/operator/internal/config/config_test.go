package config

import (
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("OPERATOR_GRPC_LISTEN", "")
	t.Setenv("OPERATOR_HEALTH_LISTEN", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GRPCListen != ":9090" {
		t.Errorf("GRPCListen = %q, want :9090", cfg.GRPCListen)
	}
	if cfg.HealthListen != ":8091" {
		t.Errorf("HealthListen = %q, want :8091", cfg.HealthListen)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("OPERATOR_GRPC_LISTEN", ":7000")
	t.Setenv("OPERATOR_HEALTH_LISTEN", ":7001")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GRPCListen != ":7000" || cfg.HealthListen != ":7001" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadVenueProofEnvDefaultsOff(t *testing.T) {
	t.Setenv("OPERATOR_VENUE_PROOF", "")
	t.Setenv("OPERATOR_OKX_BASE_URL", "")
	t.Setenv("OPERATOR_BINANCE_BASE_URL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VenueProof != "" {
		t.Errorf("VenueProof = %q, want empty (off) by default", cfg.VenueProof)
	}
	if len(cfg.VenueBaseURLs) != 0 {
		t.Errorf("VenueBaseURLs = %v, want empty when no base URL env vars are set", cfg.VenueBaseURLs)
	}
}

func TestLoadVenueProofEnvOnlyIncludesConfiguredVenues(t *testing.T) {
	t.Setenv("OPERATOR_VENUE_PROOF", "require")
	t.Setenv("OPERATOR_OKX_BASE_URL", "https://www.okx.com")
	t.Setenv("OPERATOR_BINANCE_BASE_URL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VenueProof != "require" {
		t.Errorf("VenueProof = %q, want require", cfg.VenueProof)
	}
	if got := cfg.VenueBaseURLs; len(got) != 1 || got["okx"] != "https://www.okx.com" {
		t.Errorf("VenueBaseURLs = %v, want only okx set", got)
	}
	if _, ok := cfg.VenueBaseURLs["binance"]; ok {
		t.Errorf("VenueBaseURLs must not contain binance when its env var is unset")
	}
}

func TestLoadVenueProofAcceptedValues(t *testing.T) {
	for _, v := range []string{"", "off", "require"} {
		v := v
		t.Run("value="+v, func(t *testing.T) {
			t.Setenv("OPERATOR_VENUE_PROOF", v)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(OPERATOR_VENUE_PROOF=%q): %v", v, err)
			}
			if cfg.VenueProof != v {
				t.Errorf("VenueProof = %q, want %q", cfg.VenueProof, v)
			}
		})
	}
}

func TestLoadVenueProofRejectsInvalidValues(t *testing.T) {
	for _, v := range []string{"Require", "true", "required", "1"} {
		v := v
		t.Run("value="+v, func(t *testing.T) {
			t.Setenv("OPERATOR_VENUE_PROOF", v)
			_, err := Load()
			if err == nil {
				t.Fatalf("Load(OPERATOR_VENUE_PROOF=%q): want error, got nil", v)
			}
			if !strings.Contains(err.Error(), "OPERATOR_VENUE_PROOF") {
				t.Errorf("error = %q, want it to name OPERATOR_VENUE_PROOF", err.Error())
			}
			if !strings.Contains(err.Error(), v) {
				t.Errorf("error = %q, want it to name the offending value %q", err.Error(), v)
			}
		})
	}
}

func TestLoadProvisioningEnv(t *testing.T) {
	t.Setenv("OPERATOR_PROVISIONER_IMAGE", "ghcr.io/eighred/kanz-provisioner:latest")
	t.Setenv("OPERATOR_K3S_SERVER_URL", "https://cp:6443")
	t.Setenv("OPERATOR_K3S_TOKEN", "tok")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProvisionerImage == "" || cfg.K3sServerURL == "" || cfg.K3sToken == "" {
		t.Errorf("provisioning env not loaded: %+v", cfg)
	}
}

// TestLoadProvisionerImagePullPolicyDefaultsUnset proves the "no config means no
// behaviour change" contract: with the var unset, Load must return "" so
// provision.go leaves ImagePullPolicy absent and Kubernetes applies its own
// :latest-tag default (Always) — the exact production behaviour this setting
// must not disturb.
func TestLoadProvisionerImagePullPolicyDefaultsUnset(t *testing.T) {
	t.Setenv("OPERATOR_PROVISIONER_IMAGE_PULL_POLICY", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProvisionerImagePullPolicy != "" {
		t.Errorf("ProvisionerImagePullPolicy = %q, want empty (Kubernetes default) by default", cfg.ProvisionerImagePullPolicy)
	}
}

func TestLoadProvisionerImagePullPolicyAcceptedValues(t *testing.T) {
	for _, v := range []string{"", "Always", "IfNotPresent", "Never"} {
		v := v
		t.Run("value="+v, func(t *testing.T) {
			t.Setenv("OPERATOR_PROVISIONER_IMAGE_PULL_POLICY", v)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(OPERATOR_PROVISIONER_IMAGE_PULL_POLICY=%q): %v", v, err)
			}
			if cfg.ProvisionerImagePullPolicy != v {
				t.Errorf("ProvisionerImagePullPolicy = %q, want %q", cfg.ProvisionerImagePullPolicy, v)
			}
		})
	}
}

// TestLoadProvisionerImagePullPolicyRejectsInvalidValues guards the failure
// mode a defaulting rule always risks: a typo or wrong-case value (e.g. the
// lower-camel "ifnotpresent") must fail Load() loudly rather than falling
// through to the empty default and silently leaving the pull policy unset.
func TestLoadProvisionerImagePullPolicyRejectsInvalidValues(t *testing.T) {
	for _, v := range []string{"ifnotpresent", "always", "NEVER", "Sometimes"} {
		v := v
		t.Run("value="+v, func(t *testing.T) {
			t.Setenv("OPERATOR_PROVISIONER_IMAGE_PULL_POLICY", v)
			_, err := Load()
			if err == nil {
				t.Fatalf("Load(OPERATOR_PROVISIONER_IMAGE_PULL_POLICY=%q): want error, got nil", v)
			}
			if !strings.Contains(err.Error(), "OPERATOR_PROVISIONER_IMAGE_PULL_POLICY") {
				t.Errorf("error = %q, want it to name OPERATOR_PROVISIONER_IMAGE_PULL_POLICY", err.Error())
			}
			if !strings.Contains(err.Error(), v) {
				t.Errorf("error = %q, want it to name the offending value %q", err.Error(), v)
			}
		})
	}
}
