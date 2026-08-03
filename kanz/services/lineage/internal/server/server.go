package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/lineage/internal/graph"
	"github.com/eighred/kanz/services/lineage/internal/query"
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
	// handler is mux wrapped in auth.RequirePrincipal. It is built in New rather
	// than in cmd/lineage on purpose: identity reconstruction that lives only in a
	// composition root is wiring no test in this package exercises, and this
	// service shipped for two years with that half of the seam simply absent
	// (#268). Built here, every httptest against a Server runs it.
	handler http.Handler
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
	s.handler = auth.RequirePrincipal(s.mux)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

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
// The catalog carries coverage too: an empty or short catalog after a restart is
// this pod's index, not the estate's data model, and the two must not read alike.
func (s *Server) handleDatasets(w http.ResponseWriter, _ *http.Request) {
	ds := s.graph.Datasets()
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(ds), "datasets": ds, "coverage": s.graph.Coverage(),
	})
}

func (s *Server) handleEventLineage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	prov, err := s.query.ForEvent(r.Context(), p, r.PathValue("event_id"))
	s.writeProvenance(w, r, prov, err)
}

func (s *Server) handleDatasetLineage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	ds := graph.DatasetID{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	prov, err := s.query.ForDataset(r.Context(), p, ds)
	s.writeProvenance(w, r, prov, err)
}

// principal reads the caller auth.RequirePrincipal put on the context.
//
// The middleware has already refused a request without one, so this cannot fire
// through ServeHTTP — it fires if a handler is ever mounted on a mux that is not
// wrapped. It is kept because `p, _ := auth.PrincipalFromContext(...)` and a
// governance call on the nil result is the exact code that shipped, and reading
// past the ok is what made it invisible (#268).
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok {
		s.logger.ErrorContext(r.Context(), "lineage handler reached without a principal — "+
			"auth.RequirePrincipal is not installed on this mux", "path", r.URL.Path)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return nil, false
	}
	return p, true
}

// writeProvenance maps the three provenance outcomes onto three status codes.
//
// 410 AND NOT 404 FOR AN UNRETAINED LOOKUP (#244). 404 asserts the resource does
// not exist, which is precisely the claim a bounded index has no standing to
// make, and a caller that reads only the status code — a dashboard, an alert
// rule, `curl -f` — would take it as one. Distinguishing the two in the body
// alone leaves them looking the same to every such caller, which is the defect,
// not the fix. 410 is the closest honest code: this origin no longer serves an
// answer for that identifier, and the condition will not clear on retry (503
// would promise that it might). The body says what 410 does NOT assert.
func (s *Server) writeProvenance(w http.ResponseWriter, r *http.Request, prov *query.Provenance, err error) {
	switch {
	case errors.Is(err, query.ErrNotRetained):
		writeJSON(w, http.StatusGone, s.query.Explain(graph.LookupUnknown))
	case errors.Is(err, query.ErrNotFound):
		writeJSON(w, http.StatusNotFound, s.query.Explain(graph.LookupNotObserved))
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
