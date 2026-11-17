// Package config is the MCP read plane's runtime configuration (#743).
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/eighred/kanz/internal/env"
)

// Config is the MCP service configuration, sourced from the environment.
type Config struct {
	// Listen is the MCP API port, and it is NOT 8080 for the same reason the
	// optimization service's is not (#409/#232).
	//
	// allow-observability-scrape selects every pod in kanz-services and admits a
	// list of ports, 8080 among them — so any service listening there is
	// reachable from the whole kanz-observability namespace whatever it serves.
	// This surface decides which TENANT's state an agent may read, from the
	// principal header the api-gateway injects, so a pod in the monitoring plane
	// that could reach it could choose a tenant and read its book.
	Listen string

	// MetricsListen is a SEPARATE listener for /metrics, and the separation is
	// the security boundary rather than tidiness. It is the only port
	// allow-observability-scrape needs to admit, which is what keeps the
	// tenant-scoped route closed to everything but the gateway.
	MetricsListen string

	// RiskQueryAddr is the risk-engine's query.v1 endpoint. The optional state
	// mode also reads a separately authenticated OMS projection. Both are domain
	// APIs: this plane reaches state like
	// any other caller, never a store and never a venue.
	RiskQueryAddr string
	// StateReads is selected explicitly; partial settings refuse startup.
	StateReads     bool
	Tenant         string
	OrderQueryAddr string

	// PolicyFile is the AUTH-01 policy bundle the authorizer evaluates.
	//
	// NO BUNDLE IS A REFUSAL, not an open door: the composition root builds an
	// authorizer that denies everything, so a deployment that forgot the
	// ConfigMap serves no tenant's data rather than every tenant's.
	PolicyFile string

	SPIFFESocket string
	LogLevel     slog.Level
	Source       string
}

// Load reads the configuration.
func Load() (Config, error) {
	cfg := Config{
		Listen:         env.Or("MCP_LISTEN", ":8110"),
		MetricsListen:  env.Or("MCP_METRICS_LISTEN", ":8080"),
		RiskQueryAddr:  os.Getenv("MCP_RISK_QUERY_ADDR"),
		Tenant:         os.Getenv("MCP_TENANT"),
		OrderQueryAddr: os.Getenv("MCP_ORDER_STATE_QUERY_ADDR"),
		PolicyFile:     os.Getenv("MCP_POLICY_FILE"),
		SPIFFESocket:   os.Getenv("SPIFFE_ENDPOINT_SOCKET"),
		LogLevel:       env.ParseLevelOr(os.Getenv("MCP_LOG_LEVEL"), slog.LevelInfo),
		Source:         env.Or("MCP_SOURCE", "svc/mcp"),
	}
	if selected := os.Getenv("MCP_STATE_READS_ENABLED"); selected != "" {
		var err error
		cfg.StateReads, err = strconv.ParseBool(selected)
		if err != nil {
			return Config{}, fmt.Errorf("MCP_STATE_READS_ENABLED must be a boolean")
		}
	}
	if cfg.StateReads {
		if cfg.Tenant == "" || cfg.OrderQueryAddr == "" || cfg.RiskQueryAddr == "" || cfg.SPIFFESocket == "" {
			return Config{}, fmt.Errorf("selected MCP state reads require MCP_TENANT, both domain query addresses and SPIFFE_ENDPOINT_SOCKET")
		}
	} else if cfg.Tenant != "" || cfg.OrderQueryAddr != "" {
		return Config{}, fmt.Errorf("MCP state-read settings require MCP_STATE_READS_ENABLED=true")
	}
	if cfg.Listen == cfg.MetricsListen {
		return Config{}, fmt.Errorf("mcp: MCP_LISTEN and MCP_METRICS_LISTEN must differ — sharing a " +
			"port puts the tenant-scoped MCP route on the one allow-observability-scrape admits, " +
			"which is the exposure the split exists to close (#232)")
	}
	return cfg, nil
}
