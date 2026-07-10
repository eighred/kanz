// Package server is webhook-ingest's HTTP surface: the TradingView webhook
// endpoint plus health/ready/metrics. It owns no trading logic — it reads the
// raw body and source, hands them to the ingest Pipeline, and maps the
// pipeline's typed errors to HTTP status.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/kanz-eng/kanz/services/webhook-ingest/internal/ingest"
)

const maxBodyBytes = 1 << 20 // 1 MiB

// Readiness gates traffic.
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// Server serves the webhook endpoint over an ingest pipeline.
type Server struct {
	readiness *Readiness
	pipeline  *ingest.Pipeline
	logger    *slog.Logger
	metrics   http.Handler
	mux       *http.ServeMux
}

// Option customizes the server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler.
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

// New builds the server over an ingest pipeline.
func New(readiness *Readiness, logger *slog.Logger, pipeline *ingest.Pipeline, opts ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{readiness: readiness, pipeline: pipeline, logger: logger, mux: http.NewServeMux()}
	for _, opt := range opts {
		opt(s)
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
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
	if s.metrics != nil {
		s.mux.Handle("GET /metrics", s.metrics)
	}
	s.mux.HandleFunc("POST /webhook/tradingview", s.handleWebhook)
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body failed"})
		return
	}
	res, err := s.pipeline.Process(r.Context(), body, sourceIP(r), r.Header.Get("X-Signature"))
	if err != nil {
		s.writePipelineError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"signal_id": res.SignalID,
		"orders":    res.OrderIDs,
		"count":     len(res.OrderIDs),
	})
}

// writePipelineError maps the pipeline's typed errors to HTTP status without
// leaking why an auth check failed.
func (s *Server) writePipelineError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ingest.ErrUnauthorized):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	case errors.Is(err, ingest.ErrReplayed):
		// Idempotent duplicate — acknowledge so TradingView does not retry-storm.
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate ignored"})
	case errors.Is(err, ingest.ErrHalted):
		writeJSON(w, http.StatusLocked, map[string]string{"error": "trading halted"})
	case errors.Is(err, ingest.ErrBadRequest):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		// Unresolvable size / publish failure — a real backend fault.
		s.logger.Error("webhook processing failed", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not process signal"})
	}
}

// sourceIP is the connection's source address. X-Forwarded-For is deliberately
// NOT trusted for the allowlist decision (a client can forge it); a fronting
// proxy must be handled at the L4/ingress layer, not here.
func sourceIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
