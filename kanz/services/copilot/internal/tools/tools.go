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
	"sort"
	"strings"

	"github.com/kanz-eng/kanz/pkg/auth"
	"github.com/kanz-eng/kanz/services/copilot/internal/governed"
	"github.com/kanz-eng/kanz/services/copilot/internal/llm"
	"github.com/kanz-eng/kanz/services/copilot/internal/retrieval"
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
	authz   auth.Authorizer
	client  governed.Client
	catalog retrieval.Catalog
	byName  map[string]Tool
	order   []string
}

// NewRegistry builds the governed tool set over the authorizer, governed client,
// and lineage catalog.
func NewRegistry(authz auth.Authorizer, client governed.Client, catalog retrieval.Catalog) *Registry {
	if catalog == nil {
		catalog = retrieval.IdentityCatalog{}
	}
	r := &Registry{authz: authz, client: client, catalog: catalog, byName: map[string]Tool{}}
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

// authorize is the deny-by-default gate every governed tool runs first. It
// resolves the portfolio's owning tenant (without leaking data), then asks the
// AUTH-01 authorizer; the AuditedAuthorizer records the decision to the
// observation stream. A cross-tenant or out-of-scope portfolio is denied here.
func (r *Registry) authorize(ctx context.Context, p *auth.Principal, action auth.Action, portfolioID string) (Result, bool) {
	owner, err := r.client.OwnerTenant(ctx, portfolioID)
	if err != nil {
		// Unknown to the caller — same response regardless of tenant (no oracle).
		// Still run the authorizer so the attempt is audited.
		owner = ""
	}
	dec := r.authz.Authorize(ctx, auth.Request{
		Principal: p,
		Action:    action,
		Resource:  auth.Resource{Type: auth.ResourcePortfolio, ID: portfolioID, Tenant: owner},
	})
	if !dec.Allow {
		return Result{IsError: true, Content: "not authorized: " + dec.Reason}, false
	}
	if errors.Is(err, governed.ErrUnknownPortfolio) {
		return Result{IsError: true, Content: "no governed data for portfolio " + portfolioID}, false
	}
	return Result{}, true
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
		return Result{IsError: true, Content: err.Error()}
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
		return Result{IsError: true, Content: err.Error()}
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
		return Result{IsError: true, Content: err.Error()}
	}
	return r.render(ctx, "scenario projection", reading)
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
