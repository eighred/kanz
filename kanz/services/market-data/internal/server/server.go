package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
)

// Readiness is the service's readiness gate. Liveness (/healthz) only reports
// that the process is up; readiness (/readyz) reports that it is wired up and
// safe to receive traffic. The gate starts NOT ready and flips once the store
// is reachable and the market subscriptions are live; shutdown clears it.
type Readiness struct {
	ready atomic.Bool
}

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

type Server struct {
	logger    *slog.Logger
	readiness *Readiness
	metrics   http.Handler
	mux       *http.ServeMux
}

// Option customizes the Server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler (the OBS-01a
// Provider.MetricsHandler). Omitted ⇒ /metrics 404s.
func WithMetrics(h http.Handler) Option {
	return func(s *Server) { s.metrics = h }
}

func New(readiness *Readiness, logger *slog.Logger, opts ...Option) *Server {
	s := &Server{logger: logger, readiness: readiness, mux: http.NewServeMux()}
	for _, opt := range opts {
		opt(s)
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	if s.metrics != nil {
		s.mux.Handle("GET /metrics", s.metrics)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.readiness.Ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
