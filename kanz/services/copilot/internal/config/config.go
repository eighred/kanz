package config

import (
	"log/slog"
	"os"
	"strings"

	"github.com/kanz-eng/kanz/services/copilot/internal/llm"
)

// Config is the copilot service runtime configuration. The copilot is an
// AI-analytics agent over the governed read surface, powered by Claude. The
// Claude version is NOT pinned in code — it is selected by ModelID
// (COPILOT_MODEL_ID), so a deployment switches versions without a rebuild. The
// Claude client and the governed query client default to dependency-free in-tree
// implementations (the StubModel / StubClient); the real anthropic-sdk-go client
// (for whichever version ModelID names) and the mTLS query.v1 client wire at the
// composition root behind their seams (DEBT-02).
type Config struct {
	Listen   string
	LogLevel slog.Level

	// ModelID selects the Claude version the production client calls — the seam
	// is version-agnostic, so this is the one knob that switches versions.
	// Defaults to the MAX / most-capable model (llm.DefaultModelID).
	ModelID string

	// PolicyPath is the AUTH-01b policy bundle (role→action grants). Empty ⇒ the
	// service boots with a deny-all default authorizer.
	PolicyPath string

	// LineageAddr is the LIN-01 lineage service base URL for citation resolution
	// (PARITY-04b). Empty ⇒ the dependency-free IdentityCatalog (node == event
	// id); set ⇒ the LineageCatalog resolves each source event to its governed
	// dataset node over the mesh.
	LineageAddr string

	// RiskQueryAddr is the risk-engine query.v1 gRPC address (WIRE-02b). Empty ⇒
	// the dependency-free governed.StubClient (tests / local boot); set ⇒ the real
	// governed.GRPCClient reads risk state over query.v1 and feeds the deny-by-
	// default authz gate the owning tenant + the citation seed.
	RiskQueryAddr string
	// SPIFFESocket is the SPIFFE Workload API socket (SEC-01a CSI mount). When set,
	// the query.v1 client dials over mTLS with an in-mesh peer SVID (SEC-01b);
	// empty ⇒ plaintext (local/dev).
	SPIFFESocket string

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string
}

// Load reads the configuration from the environment with production-safe
// defaults.
func Load() (Config, error) {
	return Config{
		Listen:        envOr("COPILOT_LISTEN", ":8080"),
		LogLevel:      parseLevel(os.Getenv("COPILOT_LOG_LEVEL")),
		ModelID:       envOr("COPILOT_MODEL_ID", llm.DefaultModelID),
		PolicyPath:    os.Getenv("COPILOT_POLICY_PATH"),
		LineageAddr:   os.Getenv("COPILOT_LINEAGE_ADDR"),
		RiskQueryAddr: os.Getenv("COPILOT_RISK_QUERY_ADDR"),
		SPIFFESocket:  os.Getenv("COPILOT_SPIFFE_SOCKET"),
		OTLPEndpoint:  os.Getenv("COPILOT_OTLP_ENDPOINT"),
	}, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
