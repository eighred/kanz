// Package server is the alternatives service's HTTP surface (ALT-01b): it serves
// the point-in-time fund position (committed / called / uncalled / distributed /
// NAV) and the private-asset metrics (IRR, TVPI/DPI/RVPI) over the folded
// commitment journal. State lives in a fund.Store (in-memory by default); the bus
// consumer that folds capital-call/distribution/NAV events into the journal is
// wired at the composition root behind the Store seam.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/eighred/kanz/internal/alternatives"
	"github.com/eighred/kanz/services/alternatives/internal/fund"
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
	store     fund.Store
	metrics   http.Handler
	mux       *http.ServeMux
}

// Option customizes the server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler (OBS-01a).
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

// New builds the server over a commitment journal store.
func New(readiness *Readiness, logger *slog.Logger, store fund.Store, opts ...Option) *Server {
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
	s.mux.HandleFunc("GET /v1/commitments/{id}", s.handlePosition)
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

// handlePosition returns a commitment's folded position plus its capital
// multiples and IRR.
func (s *Server) handlePosition(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pos, err := fund.Materialize(r.Context(), s.store, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := map[string]any{
		"commitment_id": pos.CommitmentID,
		"committed":     pos.Committed.FloatString(2),
		"called":        pos.Called.FloatString(2),
		"uncalled":      pos.Uncalled().FloatString(2),
		"distributed":   pos.Distributed.FloatString(2),
		"nav":           pos.NAV.FloatString(2),
	}
	// AN UNDEFINED MULTIPLE IS OMITTED, THE WAY AN UNDEFINED IRR ALREADY WAS
	// (#623). These three used to be written unconditionally, so a commitment with
	// nothing drawn served "tvpi": 0 — indistinguishable from a fund that drew
	// capital and lost all of it. The two treatments now agree, which is the point:
	// they were five lines apart in this one body and answered the same question
	// differently.
	//
	// ABSENCE IS THE SIGNAL, and it is the one this surface already uses. A client
	// reading these keys must treat a missing one as "not yet meaningful", never
	// substitute zero — the substitution is the defect.
	if mult, err := alternatives.ComputeMultiples(pos); err == nil {
		out["tvpi"] = mult.TVPI
		out["dpi"] = mult.DPI
		out["rvpi"] = mult.RVPI
	}
	if irr, err := alternatives.IRR(alternatives.IRRFlows(pos)); err == nil {
		out["irr"] = irr
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
