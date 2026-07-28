// Package server is the copilot service's HTTP surface (COPILOT-01a): a single
// governed Q&A endpoint. The authenticated AUTH-01 Principal is read from the
// request context (placed there by the AUTH-01a authentication middleware on the
// real edge); an unauthenticated request is rejected — the copilot is never
// anonymous, and every answer is tenant- and permission-scoped to the caller.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/copilot/internal/agent"
)

// Readiness gates traffic.
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// Server is the HTTP handler.
type Server struct {
	logger    *slog.Logger
	readiness *Readiness
	agent     *agent.Agent
	metrics   http.Handler
	mux       *http.ServeMux
}

// Option customizes the server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler (OBS-01a).
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

// New builds the server over a copilot agent.
func New(readiness *Readiness, logger *slog.Logger, a *agent.Agent, opts ...Option) *Server {
	s := &Server{logger: logger, readiness: readiness, agent: a, mux: http.NewServeMux()}
	for _, opt := range opts {
		opt(s)
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	if s.metrics != nil {
		s.mux.Handle("GET /metrics", s.metrics)
	}
	s.mux.HandleFunc("POST /v1/ask", s.handleAsk)
}

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

// handleAsk answers a governed question as the authenticated Principal.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	var body struct {
		Question string `json:"question"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Question == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "question is required"})
		return
	}
	ans, err := s.agent.Ask(r.Context(), p, body.Question)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	cites := make([]string, 0, len(ans.Citations))
	for _, c := range ans.Citations {
		cites = append(cites, c.String())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"answer":            ans.Text,
		"citations":         cites,
		"grounded":          ans.Grounded,
		"refused":           ans.Refused,
		"injection_flagged": ans.InjectionFlagged,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
