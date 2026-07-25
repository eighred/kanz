// Package config is the operator service's environment configuration. Most
// fields are listen coordinates or provisioning inputs; S4a adds the
// venue-key write path's backend selection (SecretBackend) and its
// destination config (Kubernetes namespace, or Vault address/token) — the
// operator reads the k8s API via its in-cluster ServiceAccount otherwise,
// which needs no config here.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config is the operator service configuration.
type Config struct {
	// GRPCListen is the address the operator.v1 gRPC server binds. The listener is
	// mutually authenticated (OPS-M2a): callers present an SVID and only the
	// identities in AllowedClients are admitted. It was previously plaintext and
	// reachable only by `kubectl port-forward`, which was both an unauthenticated
	// RPC and a routine kubectl dependency for every operator.
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

	// Control-plane access (OPS-M2a). Both are REQUIRED at startup — the operator
	// refuses to serve without them (see cmd/operator/controlplane.go). They are
	// parsed here but deliberately NOT validated here: Load() is parsing, and the
	// refusal belongs beside the server it protects, where it is unit-tested with
	// the rest of the admission rule.
	SPIFFESocket   string // SPIFFE_ENDPOINT_SOCKET — the workload-API socket
	AllowedClients string // OPERATOR_ALLOWED_CLIENTS — comma-separated SPIFFE IDs that may call
}

// validVenueProofValues are the only values OPERATOR_VENUE_PROOF accepts. Any
// other value (a typo, a case variant, a truthy-looking string) must fail
// startup rather than silently disabling the proof check — see the VenueProof
// doc comment and I-1 in the S4b review.
var validVenueProofValues = map[string]bool{"": true, "off": true, "require": true}

// Load reads the configuration from the environment, applying defaults.
func Load() (Config, error) {
	venueProof := os.Getenv("OPERATOR_VENUE_PROOF")
	if !validVenueProofValues[venueProof] {
		return Config{}, fmt.Errorf(
			"OPERATOR_VENUE_PROOF=%q is invalid; accepted values are \"\", \"off\", \"require\"",
			venueProof,
		)
	}

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

		VenueProof:    venueProof,
		VenueBaseURLs: venueBaseURLs(),

		SPIFFESocket:   os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		AllowedClients: os.Getenv("OPERATOR_ALLOWED_CLIENTS"),
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
