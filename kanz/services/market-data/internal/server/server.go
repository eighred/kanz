package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/eighred/kanz/pkg/bus"
)

// Readiness is the service's readiness gate. Liveness (/healthz) only reports
// that the process is up; readiness (/readyz) reports that it is wired up and
// safe to receive traffic. The gate starts NOT ready and flips once the store
// is reachable and the market subscriptions are live; shutdown clears it.
type Readiness struct {
	ready  atomic.Bool
	health atomic.Pointer[bus.HealthPublisher]
}

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }

// Ready reports the process-level flag ONLY — the store is reachable and the
// market subscriptions are live. Status is the full picture, and is what /readyz
// answers. The two are kept apart so that "did startup finish" stays answerable
// on its own.
func (r *Readiness) Ready() bool { return r.ready.Load() }

// TrackPublisher attaches the feed publisher's health tracker once the producer
// exists (#299).
//
// THIS SERVICE IS A HYBRID, AND THAT IS WHY THE CALL WAS NOT OBVIOUS. It both
// CONSUMES market subjects into its store (runIngest) and PUBLISHES normalized
// events onto the spine (runFeed). Until this, only the consumer half was
// represented in readiness: the feed publisher could die and the pod kept
// answering /readyz 200, because nothing it owned had stopped.
//
// #266 classified a stopped runFeed as a NON-FATAL degradation, and that call
// stands — this does not make the process exit. What changed is #298: the sibling
// mark publishers, venue-binance and venue-okx, already treat a failing publisher
// as readiness-affecting. Two services publishing the same class of data
// disagreed, and the disagreement was an accident of who wrote which main rather
// than a decision. This settles it the way the siblings already behave.
//
// WHAT IT DOES AND DOES NOT BUY, stated plainly because the honest limits are
// narrow. /healthz is a SEPARATE endpoint and stays 200, so the pod is NOT
// restarted; ingestion keeps running; and nothing downstream is cut off, since
// this server exposes only /healthz, /readyz and /metrics — there is no data API
// to withdraw. What it buys is that a dead publisher becomes VISIBLE — in
// `kubectl get pods`, to anything alerting on readiness, and to a rollout that
// will not report success — instead of being a silence that looks like health.
//
// An atomic pointer because the probe server is already serving by the time the
// feed dials a broker and builds its producer.
func (r *Readiness) TrackPublisher(h *bus.HealthPublisher) { r.health.Store(h) }

// Status reports whether the service is ready, and if not, WHY — so the endpoint
// tells an operator what it already knows rather than sending them to the logs.
//
// A nil tracker reads as ready rather than as unhealthy: the feed goroutine
// attaches it after dialing, and a service that had not got there yet would
// otherwise report a publisher failure that has not happened.
func (r *Readiness) Status() (bool, string) {
	if !r.ready.Load() {
		return false, "not ready: starting up"
	}
	h := r.health.Load()
	if h == nil {
		return true, "ready"
	}
	if ok, consecutive, lastErr := h.Status(); !ok {
		return false, fmt.Sprintf(
			"not ready: %d consecutive feed publish failures — normalized market events are NOT reaching "+
				"the spine, so every downstream consumer is pricing against a book that stopped moving "+
				"without saying so. last error: %v", consecutive, lastErr)
	}
	return true, "ready"
}

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

// handleReadyz answers from Status, not from Ready, so that a failing feed
// publisher actually reaches the probe. Reading Ready() here instead would leave
// TrackPublisher wired and inert — the shape of a check that looks present and
// decides nothing.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	ok, reason := s.readiness.Status()
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": reason})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
