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
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/eighred/kanz/internal/wealth"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/wealth/internal/book"
	"github.com/eighred/kanz/services/wealth/internal/drift"
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
	tenant    string
	store     book.Store
	metrics   http.Handler
	// drift answers "has this household moved off its model" on the read path.
	// Nil ⇒ the response says so explicitly rather than omitting the field, so an
	// advisor cannot read a missing drift block as a book that matches its model.
	drift DriftEvaluator
	mux   *http.ServeMux
}

// DriftEvaluator measures a household against the model portfolio its risk
// profile selects, WITHOUT recording anything. The read surface must not move the
// counters that measure how the book is behaving — a dashboard refresh is not a
// rebalance signal — which is why this is the pure Evaluate half of
// drift.Monitor and not its recording Observe half.
type DriftEvaluator interface {
	Evaluate(h wealth.Household) drift.Result
}

// Option customizes the server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler (OBS-01a).
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

// WithDriftMonitor attaches the WEALTH-01d drift evaluation to the household read
// (#1010). Without it the household view reports holdings, weights and asset-class
// exposure and says nothing about whether any of it matches the allocation the
// household is supposed to hold.
func WithDriftMonitor(d DriftEvaluator) Option {
	return func(s *Server) {
		if d != nil {
			s.drift = d
		}
	}
}

// New builds the server over a household composition store.
//
// tenant is the tenant THIS INSTANCE serves, and it is a required positional
// parameter rather than an Option on purpose: it is the input to the only
// cross-tenant control on this surface, and an Option can be forgotten. Same
// stance as pg.NewTenantPool, which refuses an empty tenant outright rather than
// serving something that looks healthy and is scoped to nothing.
func New(readiness *Readiness, logger *slog.Logger, tenant string, store book.Store, opts ...Option) *Server {
	s := &Server{logger: logger, readiness: readiness, tenant: tenant, store: store, mux: http.NewServeMux()}
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
	s.mux.HandleFunc("GET /v2/households/{id}", s.handleHousehold)
}

// notFoundBody is the ONE body this surface returns for "no such household",
// whether the household does not exist or belongs to another tenant. The two
// must be indistinguishable — see auth.RequireCallerTenantIs.
const notFoundBody = "household not found"

// callerOwnsThisInstance reports whether the authenticated caller may read this
// instance's data, and writes the refusal if not.
//
// THIS SURFACE HAD NO TENANT CHECK AT ALL (#222). handleHousehold read
// r.PathValue("id") and returned the household — the gateway injected the
// caller's tenant on every request and the handler never read it. Anyone who
// could reach it could enumerate household ids and read total value, holdings,
// weights and asset-class exposure for whichever tenant owned them.
//
// The check itself, the header name, and the no-oracle 404 are
// auth.RequireCallerTenantIs (#258) — the same three lines lived in four
// services and the gateway, so a change to any of them had to be made five
// times. What stays here is the only part that is this service's: the body a
// genuine miss returns, which the refusal must be identical to.
//
// Upstreams may trust that header ONLY because a NetworkPolicy makes the gateway
// their sole reachable caller. That premise is doing real work here — see #232,
// which tracks the namespaces where it is not yet enforced.
func (s *Server) callerOwnsThisInstance(w http.ResponseWriter, r *http.Request) bool {
	return auth.RequireCallerTenantIs(w, r, s.tenant, notFoundBody)
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
	w.Header().Set("Cache-Control", "no-store")
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
	id := r.PathValue("id")
	h, ok, err := s.store.Get(r.Context(), id)
	if err != nil {
		state := "unavailable"
		if errors.Is(err, wealth.ErrLegacyPrecision) {
			state = "legacy_unverified"
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "exact household valuation unavailable", "values_state": state})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": notFoundBody})
		return
	}
	vp, err := wealth.Aggregate(h)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "exact household valuation unavailable"})
		return
	}
	weights, weightsErr := vp.Weights()
	classes, classesErr := vp.AssetClassExposure()
	weightsState := "exact"
	if weightsErr != nil || classesErr != nil {
		weightsState = "unavailable"
		weights = nil
		classes = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"household_id": vp.HouseholdID,
		"total_value":  vp.TotalValue,
		"cash":         vp.Cash,
		"holdings":     vp.Holdings,
		"weights":      weights, "weights_state": weightsState,
		"asset_class": classes, "arithmetic_version": 2, "currency_code": h.CurrencyCode, "as_of": h.AsOf, "recorded_by": h.RecordedBy, "source_reason": h.Reason,
		"risk_profile": h.RiskProfile.String(),
		"drift":        s.driftView(h),
	})
}

// driftView renders the household's drift from its model portfolio for the read
// surface.
//
// IT NEVER OMITS THE KEY AND NEVER RETURNS A BARE ZERO. A response with no drift
// block, or one carrying max: 0, is read by a human as "this book matches its
// model" — which is the same confusion between "not measured" and "measured, and
// fine" that left this whole capability dark. So an unevaluated household carries
// evaluated: false, the outcome that applied, and the reason, and it carries no
// drift numbers at all.
func (s *Server) driftView(h wealth.Household) map[string]any {
	if s.drift == nil {
		return map[string]any{
			"evaluated": false,
			"outcome":   string(drift.OutcomeCatalogueUnarmed),
			"reason":    "this instance has no model catalogue wired; no target allocation is known",
		}
	}
	res := s.drift.Evaluate(h)
	if !res.Evaluated {
		return map[string]any{
			"evaluated": false,
			"outcome":   string(res.Outcome),
			"reason":    res.Reason,
		}
	}
	return map[string]any{
		"evaluated":     true,
		"outcome":       string(res.Outcome),
		"model_id":      res.ModelID,
		"tolerance":     res.Tolerance,
		"max":           res.Drift.Max,
		"total":         res.Drift.Total,
		"by_instrument": res.Drift.ByInstrument,
		"breached":      res.Breached,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
