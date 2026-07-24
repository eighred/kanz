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

	// Pre-write venue-key account proof (S4b). VenueProof "require" turns it on;
	// empty/"off" leaves S4a behaviour (write without proof). A venue with no base URL
	// configured cannot be proved, so with proof on its write is refused, never written
	// unproven. There is deliberately NO default endpoint: a wrong default would either
	// reject valid production keys (testnet) or send a rig's dummy keys to the real
	// exchange (mainnet).
	VenueProof    string            // OPERATOR_VENUE_PROOF: "" | "off" | "require"
	VenueBaseURLs map[string]string // OPERATOR_OKX_BASE_URL, OPERATOR_BINANCE_BASE_URL
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

		VenueProof:    os.Getenv("OPERATOR_VENUE_PROOF"),
		VenueBaseURLs: venueBaseURLs(),
	}, nil
}

// venueBaseURLs reads the per-venue exchange base URL env vars, including only
// the venues whose var is non-empty — there is no default endpoint (see the
// VenueBaseURLs doc comment on Config).
func venueBaseURLs() map[string]string {
	out := map[string]string{}
	if v := os.Getenv("OPERATOR_OKX_BASE_URL"); v != "" {
		out["okx"] = v
	}
	if v := os.Getenv("OPERATOR_BINANCE_BASE_URL"); v != "" {
		out["binance"] = v
	}
	return out
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
