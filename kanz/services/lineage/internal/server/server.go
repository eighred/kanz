package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/kanz-eng/kanz/pkg/auth"
	"github.com/kanz-eng/kanz/services/lineage/internal/graph"
	"github.com/kanz-eng/kanz/services/lineage/internal/query"
)

// Readiness gates traffic: starts NOT ready, flips once the graph is built and
// (when configured) the harvest subscription is live; shutdown clears it.
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

type Server struct {
	logger    *slog.Logger
	readiness *Readiness
	metrics   http.Handler
	graph     graph.Graph
	query     *query.Service
	mux       *http.ServeMux
}

type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler (OBS-01a).
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

// WithLineage mounts the catalog + provenance API over the graph and governed
// query service. Omitted ⇒ only the probes are served.
func WithLineage(g graph.Graph, q *query.Service) Option {
	return func(s *Server) { s.graph = g; s.query = q }
}

func New(readiness *Readiness, logger *slog.Logger, opts ...Option) *Server {
	s := &Server{logger: logger, readiness: readiness, mux: http.NewServeMux()}
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
	if s.graph != nil {
		s.mux.HandleFunc("GET /v1/catalog/datasets", s.handleDatasets)
	}
	if s.query != nil {
		s.mux.HandleFunc("GET /v1/lineage/event/{event_id}", s.handleEventLineage)
		s.mux.HandleFunc("GET /v1/lineage/dataset/{namespace}/{name}", s.handleDatasetLineage)
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

// handleDatasets serves the catalog listing — dataset metadata (schema ref,
// domain, last-seen). Topology metadata, not record contents, so it is not PII-
// governed; the per-dataset PII gate applies on the provenance reads below.
func (s *Server) handleDatasets(w http.ResponseWriter, _ *http.Request) {
	ds := s.graph.Datasets()
	writeJSON(w, http.StatusOK, map[string]any{"count": len(ds), "datasets": ds})
}

func (s *Server) handleEventLineage(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	prov, err := s.query.ForEvent(r.Context(), p, r.PathValue("event_id"))
	s.writeProvenance(w, r, prov, err)
}

func (s *Server) handleDatasetLineage(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	ds := graph.DatasetID{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	prov, err := s.query.ForDataset(r.Context(), p, ds)
	s.writeProvenance(w, r, prov, err)
}

func (s *Server) writeProvenance(w http.ResponseWriter, r *http.Request, prov *query.Provenance, err error) {
	switch {
	case errors.Is(err, query.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.Is(err, query.ErrForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "access to PII lineage denied"})
	case err != nil:
		s.logger.ErrorContext(r.Context(), "lineage query error", "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	default:
		writeJSON(w, http.StatusOK, prov)
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
