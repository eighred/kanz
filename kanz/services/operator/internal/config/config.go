// Package config is the operator service's environment configuration. Most
// fields are listen coordinates or provisioning inputs; S4a adds the
// venue-key write path's backend selection (SecretBackend) and its
// destination config (Kubernetes namespace, or Vault address/token) — the
// operator reads the k8s API via its in-cluster ServiceAccount otherwise,
// which needs no config here.
package config

import (
	"os"
	"strings"
)

// Config is the operator service configuration.
type Config struct {
	// GRPCListen is the address the operator.v1 gRPC server binds. It is not
	// fronted by a Service — the universe TUI reaches it via kubectl
	// port-forward, so the RBAC on pods/portforward is the access gate.
	GRPCListen string
	// HealthListen is the address the /healthz + /readyz HTTP server binds
	// (the target of the Deployment's liveness/readiness probes).
	HealthListen string

	// Node-provisioning (S2a). Empty ProvisionerImage ⇒ AddNode is unconfigured
	// and returns Unimplemented (read-only deployment).
	ProvisionerImage string
	K3sServerURL     string
	K3sToken         string

	// Venue-key write path (S4a). Empty SecretBackend ⇒ SetVenueKeys is unconfigured
	// (Unimplemented). "kube" writes k8s Secrets; "vault" writes Vault KV-v2.
	SecretBackend        string // OPERATOR_SECRET_BACKEND: "" | "kube" | "vault"
	VenueSecretNamespace string // OPERATOR_VENUE_SECRET_NAMESPACE (kube backend), default kanz-services
	VaultAddr            string // VAULT_ADDR (vault backend)
	VaultToken           string // from VAULT_TOKEN_FILE (preferred) or VAULT_TOKEN
}

// Load reads the configuration from the environment, applying defaults.
func Load() (Config, error) {
	return Config{
		GRPCListen:   envOr("OPERATOR_GRPC_LISTEN", ":9090"),
		HealthListen: envOr("OPERATOR_HEALTH_LISTEN", ":8091"),

		ProvisionerImage: os.Getenv("OPERATOR_PROVISIONER_IMAGE"),
		K3sServerURL:     os.Getenv("OPERATOR_K3S_SERVER_URL"),
		K3sToken:         os.Getenv("OPERATOR_K3S_TOKEN"),

		SecretBackend:        os.Getenv("OPERATOR_SECRET_BACKEND"),
		VenueSecretNamespace: envOr("OPERATOR_VENUE_SECRET_NAMESPACE", "kanz-services"),
		VaultAddr:            os.Getenv("VAULT_ADDR"),
		VaultToken:           readTokenOr("VAULT_TOKEN_FILE", "VAULT_TOKEN"),
	}, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// readTokenOr reads the token from the file named by $fileEnv, if set and
// readable, else falls back to $valueEnv. This mirrors the file-based secret
// mounting convention (e.g. Vault Agent injector) without requiring the
// token to ever be a plain environment variable in production.
func readTokenOr(fileEnv, valueEnv string) string {
	if path := os.Getenv(fileEnv); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			if tok := strings.TrimSpace(string(b)); tok != "" {
				return tok
			}
		}
	}
	return os.Getenv(valueEnv)
}
