// Package server is the wealth service's HTTP surface (WEALTH-01b): it aggregates
// a household's accounts into a virtual portfolio and serves the household-level
// exposure view — aggregate holdings, instrument weights, and asset-class
// exposure. State lives in a book.Store (in-memory by default); the bus consumer
// that folds account/holding state into the store is wired at the composition
// root behind the Store seam. The risk-engine recompute over the virtual
// portfolio is driven through the risk api/v* surface there too (the RISK-02
// boundary — this service produces the input, never reaches into the engine).
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/kanz-eng/kanz/internal/wealth"
	"github.com/kanz-eng/kanz/services/wealth/internal/book"
)

// Readiness gates traffic; the read endpoints are pure over the store, so the
// service is ready as soon as it is up (the flag exists for graceful shutdown).
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// Server is the HTTP handler.
type Server struct {
	logger    *slog.Logger
	readiness *Readiness
	store     book.Store
	metrics   http.Handler
	mux       *http.ServeMux
}

// Option customizes the server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler (OBS-01a).
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

// New builds the server over a household composition store.
func New(readiness *Readiness, logger *slog.Logger, store book.Store, opts ...Option) *Server {
	s := &Server{logger: logger, readiness: readiness, store: store, mux: http.NewServeMux()}
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
	s.mux.HandleFunc("GET /v1/households/{id}", s.handleHousehold)
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

// handleHousehold aggregates a household's accounts into a virtual portfolio and
// returns its total value, instrument weights, and asset-class exposure.
func (s *Server) handleHousehold(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h, ok, err := s.store.Get(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "household not found"})
		return
	}
	vp := wealth.Aggregate(h)
	writeJSON(w, http.StatusOK, map[string]any{
		"household_id": vp.HouseholdID,
		"total_value":  vp.TotalValue,
		"cash":         vp.Cash,
		"holdings":     vp.Holdings,
		"weights":      vp.Weights(),
		"asset_class":  vp.AssetClassExposure(),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
