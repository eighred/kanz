// Package server provides separate Broker API and operations handlers, plus the
// legacy shared handler retained until a coordinated listener promotion (#232).
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/eighred/kanz/services/tv-sync/internal/brokerapi"
)

// Readiness gates traffic.
type Readiness struct {
	ready atomic.Bool
	ctx   context.Context
}

// NewReadiness binds the startup barrier to the process lifetime. Cancellation
// cannot be undone by a concurrently finishing catch-up callback.
func NewReadiness(ctx context.Context) *Readiness { return &Readiness{ctx: ctx} }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() && (r.ctx == nil || r.ctx.Err() == nil) }

// Server mounts the Broker API.
type Server struct {
	readiness  *Readiness
	mux        *http.ServeMux
	operations *http.ServeMux
	legacy     *http.ServeMux
}

// Option customizes the server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler.
func WithMetrics(h http.Handler) Option {
	return func(s *Server) {
		s.operations.Handle("GET /metrics", h)
		s.legacy.Handle("GET /metrics", h)
	}
}

// New builds the server over the broker handler.
func New(readiness *Readiness, broker *brokerapi.Handler, opts ...Option) *Server {
	s := &Server{readiness: readiness, mux: http.NewServeMux(), operations: http.NewServeMux(), legacy: http.NewServeMux()}
	for _, mux := range []*http.ServeMux{s.operations, s.legacy} {
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		})
		mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
			if !s.readiness.Ready() {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		})
	}
	broker.Routes(s.mux)
	broker.Routes(s.legacy)
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.serveBroker(w, r, s.mux) }

func (s *Server) serveBroker(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if strings.HasPrefix(r.URL.Path, "/broker/") && !s.readiness.Ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
		return
	}
	next.ServeHTTP(w, r)
}

// OperationsHandler exposes no tenant data, even when callers supply principal headers.
func (s *Server) OperationsHandler() http.Handler { return s.operations }

// LegacyHandler retains the shared port until deployment explicitly opts into
// split listeners. Its existing monitoring exposure remains tracked by #232.
func (s *Server) LegacyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serveBroker(w, r, s.legacy) })
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
