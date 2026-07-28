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

	"github.com/eighred/kanz/services/webhook-ingest/internal/ingest"
)

const maxBodyBytes = 1 << 20 // 1 MiB

// Readiness gates traffic.
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// Server serves the webhook endpoint over an ingest pipeline.
type Server struct {
	readiness      *Readiness
	pipeline       *ingest.Pipeline
	logger         *slog.Logger
	metrics        http.Handler
	cloudflareOnly bool
	mux            *http.ServeMux
}

// Option customizes the server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler.
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

// WithCloudflareOnly requires every webhook to carry CF-Connecting-IP (the
// inbound path is fronted by the Cloudflare Signing Relay; combined with the
// Cloudflare-CIDR IP allowlist it locks out public traffic).
func WithCloudflareOnly(on bool) Option { return func(s *Server) { s.cloudflareOnly = on } }

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
	// M3.8: in Cloudflare-only mode, a request that did not transit the
	// Cloudflare edge (no CF-Connecting-IP) is public noise — reject before any
	// work. The peer-IP-in-Cloudflare-CIDR check is the pipeline's IP allowlist.
	if s.cloudflareOnly && r.Header.Get("CF-Connecting-IP") == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
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
	case errors.Is(err, ingest.ErrNonceStoreUnavailable):
		// We could not find out whether this alert has already traded, so we did not
		// trade it (EXEC-M17: the replay defence fails CLOSED). This must NOT be the
		// 200 "duplicate ignored" above — the alert is not a duplicate, it is
		// UNVERIFIED, and telling the sender we handled it would drop a live trading
		// signal on the floor over a Redis blink. 503 says: try again.
		s.logger.Error("replay defence unavailable — refusing the alert rather than risking a double trade", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "replay defence unavailable; retry"})
	case errors.Is(err, ingest.ErrPositionsNotArmed):
		// We do not yet know what the fund holds, so we cannot size a CLOSE (EXEC-M19b).
		// Readiness gates on the book being armed, so this should be unreachable in a healthy
		// pod — but if the replay is lost mid-flight, REFUSE. The alternative is treating "I
		// have not learned the book" as "the fund is flat", which sizes every flatten leg at
		// zero and answers 202 Accepted for a close that never sent an order.
		s.logger.Error("position book not armed — refusing the alert rather than sizing a close against an unknown book", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "position book not ready; retry"})
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
