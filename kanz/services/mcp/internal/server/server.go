package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync/atomic"

	"github.com/eighred/kanz/internal/agentgate"
	"github.com/eighred/kanz/internal/measureread"
	"github.com/eighred/kanz/pkg/auth"
)

// maxRequestBytes bounds a request body. A tools/call carries a handful of
// identifiers; anything larger is a caller doing something this surface does not
// offer.
const maxRequestBytes = 1 << 20 // 1 MiB

// Readiness gates traffic.
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// Reader is the state this plane may read, and the interface IS the boundary.
//
// EVERY METHOD IS A READ, AND THAT IS ENFORCED BY THIS TYPE HAVING NO OTHER
// KIND. There is no Submit, no Cancel, no Amend, and no venue call, so a write
// capability cannot be reached from a tool without changing this interface —
// which is a visible diff and an arch guard failure, not an oversight. #743's
// second boundary is exactly this: "MCP must never expose order placement,
// execution, cancel, amend, or any other trading capability."
//
// It is DELIBERATELY NOT the copilot's governed.Client. That client is one
// service's projection of the risk read surface; this plane projects its own,
// narrower, because AGENTS.md requires MCP be "server-side filtered and
// projected" rather than handing an agent whatever a domain API returns.
type Reader interface {
	// Measures returns the portfolio's risk measures, already projected to what
	// this plane may expose — and, per measure, WHETHER IT MAY BE STATED AT ALL.
	// The projection is internal/measureread, shared with services/copilot,
	// because "may this number be stated" is one decision and a second copy is
	// how a fix stops spreading (#757).
	Measures(ctx context.Context, portfolioID string) (measureread.Set, error)
}

// Server is the MCP HTTP surface.
type Server struct {
	logger    *slog.Logger
	readiness *Readiness
	mux       *http.ServeMux
	gate      *agentgate.Gate
	reader    Reader
	tools     []Tool
}

// Tool is one read-only capability this plane offers.
//
// ACTION IS PART OF THE DECLARATION, not decided inside the handler, so the
// authorization a tool requires is visible in the tool list rather than buried
// in whichever function serves it.
type Tool struct {
	Name        string
	Description string
	Action      auth.Action
	// invoke runs the read. It is called ONLY after the gate has allowed it.
	invoke func(ctx context.Context, r Reader, portfolioID string) (any, error)
}

// New builds the server and registers routes.
func New(readiness *Readiness, gate *agentgate.Gate, reader Reader, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{logger: logger, readiness: readiness, mux: http.NewServeMux(), gate: gate, reader: reader}
	s.tools = []Tool{{
		Name:        "get_risk_measures",
		Description: "Read the latest governed risk measures for a portfolio the caller is entitled to.",
		Action:      auth.ActionRiskRead,
		invoke: func(ctx context.Context, r Reader, portfolioID string) (any, error) {
			return r.Measures(ctx, portfolioID)
		},
	}}
	sort.Slice(s.tools, func(i, j int) bool { return s.tools[i].Name < s.tools[j].Name })
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("POST /mcp", s.handleRPC)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.readiness.Ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// handleRPC is the whole protocol surface.
//
// THE PRINCIPAL IS REQUIRED BEFORE THE ENVELOPE IS EVEN PARSED. The gateway is
// this platform's sole identity authority — it authenticates and injects
// X-Kanz-Principal-*, and upstreams trust those headers only because a
// NetworkPolicy makes the gateway their only reachable caller. An MCP server
// that authenticated its own callers would be a second identity authority, which
// is why this plane is HTTP behind the gateway rather than stdio: a stdio server
// has no gateway, no injected principal, and nothing to scope a tenant by.
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFromHeaders(r.Header)
	if !ok || principal.Subject == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "no authenticated principal — this surface is reachable only through the " +
				"api-gateway, and every tool here is scoped to the caller's tenant",
		})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusOK, fail(nil, codeParseError, "invalid JSON"))
		return
	}
	if err := req.validate(); err != nil {
		writeJSON(w, http.StatusOK, fail(req.ID, codeInvalidRequest, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, s.dispatch(r.Context(), principal, req))
}

func (s *Server) dispatch(ctx context.Context, p *auth.Principal, req request) response {
	switch req.Method {
	case "initialize":
		return ok(req.ID, s.initialize())
	case "tools/list":
		return ok(req.ID, s.listTools())
	case "tools/call":
		return s.callTool(ctx, p, req)
	default:
		// A METHOD THIS PLANE DOES NOT SERVE IS NAMED AS UNKNOWN, not silently
		// ignored. MCP defines write-side methods this server deliberately does
		// not implement; answering them with "method not found" is the honest
		// statement that they are absent by decision.
		return fail(req.ID, codeMethodNotFound, "method not served by this read plane: "+req.Method)
	}
}
