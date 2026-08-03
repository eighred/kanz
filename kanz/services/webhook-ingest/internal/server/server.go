// Package server is webhook-ingest's HTTP surface: the TradingView webhook
// endpoint plus health/ready/metrics. It owns no trading logic — it reads the
// raw body and source, hands them to the ingest Pipeline, and maps the
// pipeline's typed errors to HTTP status.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/eighred/kanz/services/webhook-ingest/internal/ingest"
)

const maxBodyBytes = 1 << 20 // 1 MiB

// nonceProbeBudget bounds the replay-defence check a /readyz request makes.
//
// The shipped readinessProbe (infra/deploy/webhook-ingest-deploy.yaml) sets no
// timeoutSeconds, so the kubelet's budget is the 1s default. Answering inside it
// matters: a handler the kubelet kills is recorded as a failed probe with NO
// REASON, and the reason is the entire point of this check. A Redis that cannot
// answer a PING in 900ms cannot answer a claim on the trading path either, so a
// timeout here is a correct "not ready", not a false one.
const nonceProbeBudget = 900 * time.Millisecond

// NonceStoreHealth is a replay defence that can be asked whether it is reachable.
//
// It is OPTIONAL, and deliberately so: the in-process store cannot be
// unreachable — it is a map in this address space — so there is nothing for it
// to implement. Only the cross-pod (Redis) store, which is behind `-tags redis`,
// implements it. That keeps go-redis out of the default build while still
// letting readiness represent the store this pod actually has.
type NonceStoreHealth interface {
	// NonceStoreHealthy returns nil when a claim could be made right now, and the
	// reason it could not otherwise.
	NonceStoreHealthy(ctx context.Context) error
}

// Readiness gates traffic. It answers "can this pod ADMIT AN ALERT", not "is the
// process up" — and both of the things that can stop it are represented here.
//
// The second one was missing, and its absence was a total trading outage that
// every health signal called healthy. With Redis down, newNonceStore returned a
// client that had allocated a pool and connected to nothing, the position replay
// still armed, /readyz answered 200, the pod joined its Service — and then every
// TradingView alert it received was refused with ErrNonceStoreUnavailable. The
// refusal is correct (the replay defence fails CLOSED); reporting healthy while
// doing it is not, on the one service the internet talks to. Nothing paged,
// because nothing was unhealthy: the pod was Ready, the endpoint was in the
// Service, and the alerts were 503s that only TradingView ever saw.
//
// So the nonce store is readiness, the same way publish health is readiness for
// a venue adapter (internal/venueadapter/server/probes.go): a pod that cannot
// tell whether an alert already traded drops out of its Service and says why.
type Readiness struct {
	ready  atomic.Bool
	nonces atomic.Value // nonceHealthHolder — always this concrete type
}

// nonceHealthHolder keeps atomic.Value's stored type constant even though the
// interface value inside it varies by build.
type nonceHealthHolder struct{ h NonceStoreHealth }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }

// Ready reports the process-level flag only — the position book is armed. It is
// NOT the whole answer; Status is. Kept because callers already read it, and
// because a probe must be able to distinguish "still replaying" from "the replay
// defence is down".
func (r *Readiness) Ready() bool { return r.ready.Load() }

// TrackNonceStore attaches the replay defence's health check.
//
// Attached after construction rather than passed in, because the store is built
// before the probe server in main and the handlers are already serving by the
// time the book arms. A nil h is a no-op, so the default (in-process) build,
// whose store cannot be unreachable, simply never calls it.
func (r *Readiness) TrackNonceStore(h NonceStoreHealth) {
	if h == nil {
		return
	}
	r.nonces.Store(nonceHealthHolder{h: h})
}

// Status reports whether this pod can admit an alert, and if not, WHY — so the
// endpoint tells an operator what it already knows instead of sending them to
// the logs of a pod that looks fine.
func (r *Readiness) Status(ctx context.Context) (bool, string) {
	if !r.ready.Load() {
		return false, "not ready: the position book has not been learned yet"
	}
	held, ok := r.nonces.Load().(nonceHealthHolder)
	if !ok || held.h == nil {
		return true, "ready"
	}
	probeCtx, cancel := context.WithTimeout(ctx, nonceProbeBudget)
	defer cancel()
	if err := held.h.NonceStoreHealthy(probeCtx); err != nil {
		return false, fmt.Sprintf(
			"not ready: the replay defence is unreachable, so EVERY TradingView alert to this pod would be "+
				"refused (503) rather than traded — a total ingest outage. last error: %v", err)
	}
	return true, "ready"
}

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
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ok, reason := s.readiness.Status(r.Context())
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": reason})
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
