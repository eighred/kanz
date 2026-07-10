// Package server is tv-sync's HTTP surface: the TradingView Broker-API routes
// plus health/ready/metrics.
package server

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

	"github.com/kanz-eng/kanz/services/tv-sync/internal/brokerapi"
)

// Readiness gates traffic.
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// Server mounts the Broker API.
type Server struct {
	readiness *Readiness
	mux       *http.ServeMux
}

// Option customizes the server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler.
func WithMetrics(h http.Handler) Option {
	return func(s *Server) { s.mux.Handle("GET /metrics", h) }
}

// New builds the server over the broker handler.
func New(readiness *Readiness, broker *brokerapi.Handler, opts ...Option) *Server {
	s := &Server{readiness: readiness, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.readiness.Ready() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	broker.Routes(s.mux)
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
