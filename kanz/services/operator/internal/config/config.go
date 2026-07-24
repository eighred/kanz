// Package config is the operator service's environment configuration. Every
// field is a listen coordinate — there is no write target, no credential, and
// no exchange/venue config in this slice (the operator reads the k8s API via
// its in-cluster ServiceAccount, which needs no config here).
package config

import "os"

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
}

// Load reads the configuration from the environment, applying defaults.
func Load() (Config, error) {
	return Config{
		GRPCListen:   envOr("OPERATOR_GRPC_LISTEN", ":9090"),
		HealthListen: envOr("OPERATOR_HEALTH_LISTEN", ":8091"),

		ProvisionerImage: os.Getenv("OPERATOR_PROVISIONER_IMAGE"),
		K3sServerURL:     os.Getenv("OPERATOR_K3S_SERVER_URL"),
		K3sToken:         os.Getenv("OPERATOR_K3S_TOKEN"),
	}, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
