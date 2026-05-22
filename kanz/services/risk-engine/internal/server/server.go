package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
)

// Readiness is the service's readiness gate. Liveness (/healthz) only reports
// that the process is up; readiness (/readyz) reports that it is wired up and
// safe to receive traffic. The gate starts NOT ready: ORCH-01c/01d flip it
// once bus consumers are subscribed and bootstrap replay (PERS-01) has caught
// up, and ORCH-01e clears it on shutdown so a draining pod stops getting work.
type Readiness struct {
	ready atomic.Bool
}

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

type Server struct {
	logger    *slog.Logger
	readiness *Readiness
	mux       *http.ServeMux
}

func New(readiness *Readiness, logger *slog.Logger) *Server {
	s := &Server{logger: logger, readiness: readiness, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
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
