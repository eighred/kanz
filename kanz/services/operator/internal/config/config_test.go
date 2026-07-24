package config

import "testing"

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

func TestLoadProvisioningEnv(t *testing.T) {
	t.Setenv("OPERATOR_PROVISIONER_IMAGE", "ghcr.io/kanz-eng/kanz-provisioner:latest")
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
