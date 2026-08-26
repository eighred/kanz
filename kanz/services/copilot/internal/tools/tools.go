// Package tools is the copilot's tool/function layer (COPILOT-01b): it exposes
// the governed read surface (risk measures, exposure, scenario evaluation) to
// Claude as tool-use functions. Every tool invocation is gated by the AUTH-01
// authorizer (deny-by-default, COPILOT-01d) BEFORE it touches the governed
// client, so the model can never read data the calling Principal could not read
// directly — and because the authorizer is an AuditedAuthorizer, every allow AND
// deny is logged to the AUDIT-01 observation stream for free. Each result
// carries a Citation (COPILOT-01c) tracing the value to its source event.
package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/governed"
	"github.com/eighred/kanz/services/copilot/internal/llm"
	"github.com/eighred/kanz/services/copilot/internal/retrieval"
)

// Result is a tool invocation's outcome: the text the model reads, the citations
// the values trace to, and the raw numeric values (for the grounding check).
type Result struct {
	Content   string
	Citations []retrieval.Citation
	Values    []float64
	IsError   bool
}

// Tool is one governed function the model may call.
type Tool struct {
	Def    llm.ToolDef
	invoke func(ctx context.Context, p *auth.Principal, input map[string]any) Result
}

// Registry holds the tool set and dispatches calls.
type Registry struct {
	authGate *Gate
	client   governed.Client
	catalog  retrieval.Catalog
	logger   *slog.Logger
	byName   map[string]Tool
	order    []string
}

// NewRegistry builds the governed tool set over the authorizer, governed client,
// and lineage catalog.
//
// The logger is not optional in effect: a refusal's real reason is written here
// and NOWHERE the caller can see, so an operator investigating "why did the
// copilot say there is no data" has this line or nothing. A nil logger falls
// back to slog.Default() rather than dropping the record.
func NewRegistry(authz auth.Authorizer, client governed.Client, catalog retrieval.Catalog, logger *slog.Logger) *Registry {
	if catalog == nil {
		catalog = retrieval.IdentityCatalog{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &Registry{authGate: NewGate(authz, governedOwner{client: client}, logger), client: client, catalog: catalog, logger: logger, byName: map[string]Tool{}}
	r.register(Tool{
		Def: llm.ToolDef{
			Name:        "get_risk_measures",
			Description: "Get the latest governed risk measures (e.g. VaR99, Delta) for a portfolio. Call this when the user asks about a portfolio's risk numbers.",
			InputSchema: portfolioSchema("measures", "Optional measure names to filter to, e.g. [\"VaR99\"]."),
		},
		invoke: r.getMeasures,
	})
	r.register(Tool{
		Def: llm.ToolDef{
			Name:        "get_exposure",
			Description: "Get the latest governed exposure aggregate for a portfolio. Call this when the user asks how a portfolio is exposed (by asset class, sector, currency).",
			InputSchema: portfolioSchema("", ""),
		},
		invoke: r.getExposure,
	})
	r.register(Tool{
		Def: llm.ToolDef{
			Name:        "evaluate_scenario",
			Description: "Evaluate a named stress scenario against a portfolio and return the projected measures. Call this for what-if / stress questions.",
			InputSchema: scenarioSchema(),
		},
		invoke: r.evaluateScenario,
	})
	return r
}

func (r *Registry) register(t Tool) {
	r.byName[t.Def.Name] = t
	r.order = append(r.order, t.Def.Name)
}

// Defs returns the tool definitions for the model request, in registration order.
func (r *Registry) Defs() []llm.ToolDef {
	out := make([]llm.ToolDef, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n].Def)
	}
	return out
}

// Invoke dispatches a tool call under the Principal. An unknown tool, a missing
// portfolio, or a denied authorization all return an IsError result the model
// can read and adapt to (never a panic, never silent data).
func (r *Registry) Invoke(ctx context.Context, p *auth.Principal, call llm.ToolCall) Result {
	t, ok := r.byName[call.Name]
	if !ok {
		return Result{IsError: true, Content: fmt.Sprintf("unknown tool %q", call.Name)}
	}
	return t.invoke(ctx, p, call.Input)
}

// Refusal text a tool may put in front of the model. It is a CLOSED SET chosen
// by deny code — never a passthrough of the authorizer's Reason, which names the
// resource's owning tenant on a cross-tenant deny and used to be concatenated
// straight into this content (#741).
//
// msgNoGovernedData deliberately answers TWO different questions with one
// sentence: the portfolio does not exist, and the portfolio belongs to another
// tenant. A caller able to tell those apart can enumerate another tenant's
// portfolios by id, which is the discovery CLAUDE.md puts out of reach ("one
// tenant's agent must not be able to discover another's state"). The identical
// wording is the control; keep it identical. Both arrive here as isolation deny
// codes — the unknown portfolio because an unresolvable owner leaves the
// resource tenant empty, which is itself an isolation refusal.
const (
	msgNoGovernedData = "no governed data for portfolio "
	msgNotAuthorized  = "not authorized to read this portfolio"
	// msgOwnerUnavailable is NOT collapsed into msgNoGovernedData. A failed
	// ownership lookup is an outage of the governed read surface — uncorrelated
	// with any tenant, so stating it is not an oracle — and presenting an outage
	// as an empty portfolio is the silent-default this estate refuses.
	msgOwnerUnavailable = "the portfolio's owning tenant could not be established, so this read is refused"
	// msgReadFailed stands in for a governed read that failed AFTER
	// authorization. The dependency's own error text is logged, not rendered.
	msgReadFailed = "the governed read surface could not be read right now"
)

// authorize delegates to the Gate, which owns the decision (#743).
//
// THE PORTFOLIO IS THE RESOURCE TYPE, and naming it here rather than inside the
// gate is what lets the same gate serve a second tool surface without a second
// copy of the decision.
func (r *Registry) authorize(ctx context.Context, p *auth.Principal, action auth.Action, portfolioID string) (Result, bool) {
	v := r.authGate.Authorize(ctx, p, action, auth.ResourcePortfolio, portfolioID)
	if !v.Allowed {
		return Result{IsError: true, Content: v.Message}, false
	}
	return Result{}, true
}

// governedOwner adapts the governed read surface to the gate's OwnerResolver.
//
// IT EXISTS TO MAP ONE SENTINEL, and that is the whole seam: the gate must not
// import the copilot's data source to know what "not visible" means, or the
// decision travels with this service's particular reader. governed's
// ErrUnknownPortfolio is already tenant-indistinguishable by construction, which
// is exactly the contract ErrResourceNotVisible states.
type governedOwner struct{ client governed.Client }

func (g governedOwner) OwnerTenant(ctx context.Context, resourceID string) (string, error) {
	tenant, err := g.client.OwnerTenant(ctx, resourceID)
	if errors.Is(err, governed.ErrUnknownPortfolio) {
		return "", ErrResourceNotVisible
	}
	return tenant, err
}

func (r *Registry) getMeasures(ctx context.Context, p *auth.Principal, in map[string]any) Result {
	pid := str(in["portfolio_id"])
	if pid == "" {
		return Result{IsError: true, Content: "portfolio_id is required"}
	}
	if res, ok := r.authorize(ctx, p, auth.ActionRiskRead, pid); !ok {
		return res
	}
	reading, err := r.client.Measures(ctx, pid, strSlice(in["measures"]))
	if err != nil {
		return r.readFailed(ctx, "measures", pid, err)
	}
	return r.render(ctx, "risk measures", reading)
}

func (r *Registry) getExposure(ctx context.Context, p *auth.Principal, in map[string]any) Result {
	pid := str(in["portfolio_id"])
	if pid == "" {
		return Result{IsError: true, Content: "portfolio_id is required"}
	}
	if res, ok := r.authorize(ctx, p, auth.ActionRiskRead, pid); !ok {
		return res
	}
	reading, err := r.client.Exposure(ctx, pid)
	if err != nil {
		return r.readFailed(ctx, "exposure", pid, err)
	}
	return r.render(ctx, "exposure", reading)
}

func (r *Registry) evaluateScenario(ctx context.Context, p *auth.Principal, in map[string]any) Result {
	pid := str(in["portfolio_id"])
	if pid == "" {
		return Result{IsError: true, Content: "portfolio_id is required"}
	}
	if res, ok := r.authorize(ctx, p, auth.ActionRiskScenario, pid); !ok {
		return res
	}
	reading, err := r.client.EvaluateScenario(ctx, pid, str(in["scenario"]))
	if err != nil {
		return r.readFailed(ctx, "scenario", pid, err)
	}
	return r.render(ctx, "scenario projection", reading)
}

// readFailed renders a POST-AUTHORIZATION read failure. The caller is known to
// own this portfolio by the time it can get here, so this is not an isolation
// boundary — but the governed client's error is still the read surface's own
// words (a gRPC status carrying a backend message), and it used to be handed to
// the model verbatim. Same rule as the refusals above: the dependency's text
// goes to the log, the model gets a fixed sentence.
func (r *Registry) readFailed(ctx context.Context, kind, portfolioID string, err error) Result {
	// ErrUnknownPortfolio here means the ownership lookup resolved but the
	// reading did not — a portfolio with no data of this kind, not an outage.
	if errors.Is(err, governed.ErrUnknownPortfolio) {
		return Result{IsError: true, Content: msgNoGovernedData + portfolioID}
	}
	r.logger.WarnContext(ctx, "copilot: governed read failed",
		"portfolio_id", portfolioID, "kind", kind, "err", err)
	return Result{IsError: true, Content: msgReadFailed}
}

// render formats a governed reading into tool-result text + a citation. The
// citation resolves the source event through the LIN-01 catalog.
func (r *Registry) render(ctx context.Context, label string, reading governed.Reading) Result {
	node, _ := r.catalog.Resolve(ctx, reading.SourceEventID)
	asOf := reading.AsOf.UTC().Format("2006-01-02T15:04:05Z")
	cite := retrieval.Citation{
		SourceEventID: reading.SourceEventID,
		PortfolioID:   reading.PortfolioID,
		AsOf:          asOf,
		LineageNode:   node,
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s for %s (as of %s):\n", label, reading.PortfolioID, asOf)
	var values []float64
	for _, name := range reading.OrderedNames() {
		v := reading.Values[name]
		fmt.Fprintf(&b, "- %s = %g\n", name, v)
		values = append(values, v)
	}
	fmt.Fprintf(&b, "source: %s", cite.String())
	return Result{Content: b.String(), Citations: []retrieval.Citation{cite}, Values: values}
}

func portfolioSchema(extra, extraDesc string) map[string]any {
	props := map[string]any{
		"portfolio_id": map[string]any{"type": "string", "description": "The portfolio to query."},
	}
	required := []any{"portfolio_id"}
	if extra == "measures" {
		props["measures"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": extraDesc}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

func scenarioSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"portfolio_id": map[string]any{"type": "string", "description": "The portfolio to stress."},
			"scenario":     map[string]any{"type": "string", "description": "The named scenario, e.g. \"GFC\" or \"RATES_+100BP\"."},
		},
		"required":             []any{"portfolio_id", "scenario"},
		"additionalProperties": false,
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
