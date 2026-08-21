package config

import (
	"github.com/eighred/kanz/internal/env"
	"log/slog"
	"os"
	"strings"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/pkg/secret"
	"github.com/eighred/kanz/services/copilot/internal/llm"
)

// DefaultOpenRouterBaseURL is OpenRouter's OpenAI-compatible endpoint.
const DefaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"

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
	// Provider selects which linked LLM adapter answers (#179), from
	// COPILOT_PROVIDER. Required, with no default: which model answers a
	// portfolio manager's question is not a decision to make by omission, and
	// what is available depends on the build tags this binary was compiled with.
	// See cmd/copilot/model.go for the resolution and the fail-closed rule.
	Provider string

	ModelID string

	// AnthropicAPIKey authenticates the Claude client. Sourced from a SEC-01d
	// Vault/CSI secret FILE (COPILOT_ANTHROPIC_API_KEY_FILE), never a plaintext env
	// var — it is a billable credential.
	AnthropicAPIKey string

	// OpenRouterAPIKey authenticates the OpenRouter client (#179). Same SEC-01d
	// file-mount rule as the Anthropic key: it is a billable credential, so it
	// arrives as COPILOT_OPENROUTER_API_KEY_FILE, never a plaintext env var.
	OpenRouterAPIKey string

	// OpenRouterBaseURL overrides the OpenRouter endpoint
	// (COPILOT_OPENROUTER_BASE_URL). Defaults to the real API.
	//
	// IT EXISTS SO THE ADAPTER IS TESTABLE WITHOUT A KEY OR A NETWORK. Every
	// mapping test points it at an httptest server; CI must never depend on a
	// live API being reachable or a key being present. That it also allows a
	// proxy or a self-hosted gateway is a side benefit, not the reason.
	OpenRouterBaseURL string

	// AllowStub permits the StubModel to serve. OFF by default; must be set
	// deliberately.
	//
	// The stub does not answer questions — it FABRICATES answers, and they reach a
	// portfolio manager as analysis, reading exactly like real ones. This is the
	// copilot twin of market-ingest's SimFeed and the OMS's SimVenue, and it breaks
	// the same rule: never inject data. It must not be reachable by forgetting a
	// build tag.
	AllowStub bool

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
	// the query.v1 client dials over mTLS with an in-mesh peer SVID (SEC-01b), and
	// the bus dials the spine presenting the same workload SVID; empty ⇒ plaintext
	// (local/dev), which the production broker refuses at the handshake (SEC-M3).
	SPIFFESocket string

	// NATSURL is the spine the AUTH-01d decision recorder publishes to (#352).
	// Empty ⇒ decisions are recorded to the LOG ONLY, which is where this service
	// started: a tool authorization that exists solely in a pod's stdout is gone at
	// the next rollout and cannot be queried beside the FACTs it justified.
	//
	// It is the copilot's ONLY bus interaction — it publishes decisions and
	// subscribes to nothing.
	NATSURL string

	// Tenant is the fallback tenant_id for a decision that carries NO principal
	// tenant of its own. It is a FALLBACK and not the tenant: pkg/authbus stamps
	// each decision with the deciding principal's tenant when there is one, so a
	// decision about acme's analyst is filed under acme rather than under whatever
	// this deployment was configured with.
	//
	// Defaults to bus.SystemTenant. A tool authorization made with no principal at
	// all is a platform-level event, not any customer's — and bus.Validate rejects
	// an empty tenant_id on the live path, so without a value here those decisions
	// would be refused rather than recorded.
	Tenant string

	// OTLPEndpoint is the OTel collector for span export (OBS-01). Empty ⇒ none.
	OTLPEndpoint string
}

// Load reads the configuration from the environment with production-safe
// defaults.
func Load() (Config, error) {
	// Resolved before the literal so an unreadable COPILOT_ANTHROPIC_API_KEY_FILE
	// stops Load HERE. The local helper this replaces fell through to "" on a bad
	// mount, and an empty API key does not fail where it can be seen: the process
	// starts clean and the deploy goes green, then the first analyst question comes
	// back as an auth error from Anthropic with nothing pointing at the mount that
	// never landed. See pkg/secret.
	anthropicAPIKey, err := secret.Read("COPILOT_ANTHROPIC_API_KEY")
	if err != nil {
		return Config{}, err
	}

	// The OpenRouter key is optional at LOAD time: a binary that never selects
	// that provider must not be forced to mount a credential for it. The adapter
	// refuses at build time when it is selected without one — which is where the
	// requirement actually is.
	openRouterAPIKey, err := secret.Read("COPILOT_OPENROUTER_API_KEY")
	if err != nil {
		return Config{}, err
	}

	return Config{
		Listen:          env.Or("COPILOT_LISTEN", ":8080"),
		LogLevel:        env.ParseLevelOr(os.Getenv("COPILOT_LOG_LEVEL"), slog.LevelInfo),
		ModelID:         env.Or("COPILOT_MODEL_ID", llm.DefaultModelID),
		AnthropicAPIKey: anthropicAPIKey,
		Provider:        strings.TrimSpace(os.Getenv("COPILOT_PROVIDER")),
		AllowStub:       os.Getenv("COPILOT_ALLOW_STUB") == "true",

		OpenRouterAPIKey:  openRouterAPIKey,
		OpenRouterBaseURL: env.Or("COPILOT_OPENROUTER_BASE_URL", DefaultOpenRouterBaseURL),
		PolicyPath:        os.Getenv("COPILOT_POLICY_PATH"),
		LineageAddr:       os.Getenv("COPILOT_LINEAGE_ADDR"),
		RiskQueryAddr:     os.Getenv("COPILOT_RISK_QUERY_ADDR"),
		SPIFFESocket:      os.Getenv("COPILOT_SPIFFE_SOCKET"),
		OTLPEndpoint:      os.Getenv("COPILOT_OTLP_ENDPOINT"),
		NATSURL:           os.Getenv("COPILOT_NATS_URL"),
		Tenant:            env.Or("COPILOT_TENANT", bus.SystemTenant),
	}, nil
}
