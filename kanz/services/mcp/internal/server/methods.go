package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/eighred/kanz/pkg/auth"
)

// protocolVersion is the MCP revision this surface implements. It is stated
// rather than negotiated: a caller speaking a different revision is better told
// which one it reached than served a best guess.
const protocolVersion = "2025-06-18"

// initializeResult answers the handshake.
//
// CAPABILITIES NAMES ONLY WHAT IS SERVED, AND THE ABSENCES ARE THE POINT. There
// is no "resources", no "prompts", no "sampling" and no write-side capability
// here — not because they are unimplemented and might appear, but because this
// plane is read-only by decision (#743). readOnly is stated explicitly for the
// same reason: MCP reports capability by omission, and an omission is
// indistinguishable from a server that simply did not mention it. This estate's
// rule is that "empty means DID NOT SAY, never SUPPORTS NOTHING" (#675), so the
// answer says what it is rather than leaving a reader to infer it from silence.
type initializeResult struct {
	ProtocolVersion string           `json:"protocolVersion"`
	ServerInfo      serverInfo       `json:"serverInfo"`
	Capabilities    capabilitiesInfo `json:"capabilities"`
}

type serverInfo struct {
	Name string `json:"name"`
}

type capabilitiesInfo struct {
	Tools toolsCapability `json:"tools"`
	// ReadOnly is not part of the MCP schema. It is stated anyway, because a
	// caller reading this plane's capabilities should not have to conclude
	// "no write tools exist" from their absence.
	ReadOnly bool `json:"readOnly"`
}

type toolsCapability struct {
	// ListChanged is false and stays false: the tool set is fixed at
	// construction, so a client never needs to re-list. A plane that could grow
	// a capability at runtime is one whose boundaries are decided at runtime.
	ListChanged bool `json:"listChanged"`
}

func (s *Server) initialize() initializeResult {
	return initializeResult{
		ProtocolVersion: protocolVersion,
		ServerInfo:      serverInfo{Name: "kanz-mcp-read-plane"},
		Capabilities:    capabilitiesInfo{Tools: toolsCapability{ListChanged: false}, ReadOnly: true},
	}
}

// toolDescriptor is one entry in tools/list.
type toolDescriptor struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type toolsListResult struct {
	Tools []toolDescriptor `json:"tools"`
}

// listTools renders the tool set.
//
// EVERY TOOL HERE IS A READ, and the list is built from the same slice the
// dispatcher runs — not a second declaration that could drift from it. A tool
// that appears here and cannot be called, or can be called and does not appear,
// is the capability-drift defect this estate has paid for five times (#240,
// #405, #486, #675, #742).
func (s *Server) listTools() toolsListResult {
	out := make([]toolDescriptor, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, toolDescriptor{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"portfolio_id": map[string]any{
						"type":        "string",
						"description": "The portfolio to read. Scoped to the caller's tenant.",
					},
				},
				"required": []string{"portfolio_id"},
			},
		})
	}
	return toolsListResult{Tools: out}
}

type callParams struct {
	Name      string `json:"name"`
	Arguments struct {
		PortfolioID string `json:"portfolio_id"`
	} `json:"arguments"`
}

// callToolResult is the MCP tool result shape. IsError carries a refusal the
// caller may read; the content is always from a closed set.
type callToolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func textResult(s string, isErr bool) callToolResult {
	return callToolResult{Content: []contentBlock{{Type: "text", Text: s}}, IsError: isErr}
}

// callTool runs one read, and NOT ONE BYTE OF IT HAPPENS BEFORE THE GATE.
//
// The gate is internal/agentgate — the same decision services/copilot runs,
// promoted to shared code when this plane became its second consumer rather than
// copied. That matters more here than anywhere: a second copy of a tenant check
// is how a fix stops spreading, and the fix this one carries is #741, where an
// unresolved resource tenant used to skip the cross-tenant boundary entirely.
func (s *Server) callTool(ctx context.Context, p *auth.Principal, req request) response {
	var params callParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return fail(req.ID, codeInvalidParams, "invalid params")
	}
	if params.Arguments.PortfolioID == "" {
		return fail(req.ID, codeInvalidParams, "portfolio_id is required")
	}
	tool, found := s.tool(params.Name)
	if !found {
		// Named as unknown rather than refused: which tools exist is not tenant
		// state, and tools/list already says so.
		return fail(req.ID, codeMethodNotFound, "unknown tool: "+params.Name)
	}

	v := s.gate.Authorize(ctx, p, tool.Action, auth.ResourcePortfolio, params.Arguments.PortfolioID)
	if !v.Allowed {
		// The gate's message is already the closed-set rendering. The
		// authorizer's own reason names the owning tenant and goes to the log
		// and the audit stream inside the gate, never here (#741).
		return fail(req.ID, codeUnauthorized, v.Message)
	}

	out, err := tool.invoke(ctx, s.reader, params.Arguments.PortfolioID)
	if err != nil {
		// The dependency's own words are logged, not rendered: a read surface's
		// error text is not this plane's to publish to an agent.
		s.logger.WarnContext(ctx, "mcp: governed read failed",
			"tool", tool.Name, "portfolio_id", params.Arguments.PortfolioID, "err", err)
		return ok(req.ID, textResult("the governed read surface could not be read right now", true))
	}
	body, err := json.Marshal(out)
	if err != nil {
		return fail(req.ID, codeInternalError, "result could not be encoded")
	}
	return ok(req.ID, textResult(string(body), false))
}

func (s *Server) tool(name string) (Tool, bool) {
	for _, t := range s.tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
