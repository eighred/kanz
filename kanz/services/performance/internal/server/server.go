// Package server is the performance service's HTTP surface (PERF-01c): a
// stateless analytics endpoint over internal/performance. Each handler decodes a
// JSON request carrying the measurement inputs and returns the computed result —
// time-/money-weighted returns, benchmark-relative active return, Brinson
// attribution, and ex-post risk. The point-in-time valuation that feeds these
// from the bitemporal store is the internal/performance Valuer (tested there);
// this surface is the reporting layer a PM/UI calls.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	perf "github.com/eighred/kanz/internal/performance"
)

// Readiness gates traffic: the analytics endpoints are pure compute, so the
// service is ready as soon as it is up; the flag exists for graceful shutdown.
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// Server is the HTTP handler.
type Server struct {
	logger    *slog.Logger
	readiness *Readiness
	metrics   http.Handler
	mux       *http.ServeMux
}

// Option customizes the server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler (OBS-01a).
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

// New builds the server and registers routes.
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
	s.mux.HandleFunc("POST /v1/returns", s.handleReturns)
	s.mux.HandleFunc("POST /v1/attribution", s.handleAttribution)
	s.mux.HandleFunc("POST /v1/risk", s.handleRisk)
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

// --- returns -----------------------------------------------------------------

// returnsRequest carries a flow-aware return computation. SubPeriods drive the
// time-weighted return; the Period fields drive the money-weighted return. A
// caller may supply either or both.
type returnsRequest struct {
	PortfolioID string           `json:"portfolio_id"`
	Start       time.Time        `json:"start"`
	End         time.Time        `json:"end"`
	SubPeriods  []perf.SubPeriod `json:"sub_periods"`
	BeginValue  float64          `json:"begin_value"`
	EndValue    float64          `json:"end_value"`
	Flows       []perf.Flow      `json:"flows"`
	AsOf        time.Time        `json:"as_of"`
}

type returnsResponse struct {
	PortfolioID      string  `json:"portfolio_id"`
	TimeWeighted     float64 `json:"time_weighted"`
	MoneyWeighted    float64 `json:"money_weighted"`     // Modified Dietz
	MoneyWeightedIRR float64 `json:"money_weighted_irr"` // true IRR
	AnnualizedTWR    float64 `json:"annualized_twr"`
	WindowYears      float64 `json:"window_years"`
}

func (s *Server) handleReturns(w http.ResponseWriter, r *http.Request) {
	var req returnsRequest
	if !decode(w, r, &req) {
		return
	}
	twr := perf.TimeWeightedReturn(req.SubPeriods)
	period := perf.Period{Start: req.Start, End: req.End, BeginValue: req.BeginValue, EndValue: req.EndValue, Flows: req.Flows}
	years := 0.0
	if !req.End.IsZero() && req.End.After(req.Start) {
		years = req.End.Sub(req.Start).Hours() / 24 / 365
	}
	resp := returnsResponse{
		PortfolioID:      req.PortfolioID,
		TimeWeighted:     twr,
		MoneyWeighted:    period.ModifiedDietz(),
		MoneyWeightedIRR: period.IRR(),
		WindowYears:      years,
	}
	if years >= 1 {
		resp.AnnualizedTWR = perf.Annualize(twr, years)
	} else {
		resp.AnnualizedTWR = twr
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- attribution -------------------------------------------------------------

type attributionRequest struct {
	PortfolioID string               `json:"portfolio_id"`
	BenchmarkID string               `json:"benchmark_id"`
	Periods     []perf.PeriodBrinson `json:"periods"`
	// Sectors is the single-period convenience form: when Periods is empty the
	// server runs a one-period Brinson over these sector rows.
	Sectors []perf.SectorData `json:"sectors"`
}

func (s *Server) handleAttribution(w http.ResponseWriter, r *http.Request) {
	var req attributionRequest
	if !decode(w, r, &req) {
		return
	}
	var result perf.LinkedAttribution
	if len(req.Periods) > 0 {
		result = perf.LinkCarino(req.Periods)
	} else {
		result = perf.SingleAttribution(req.Sectors)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"portfolio_id": req.PortfolioID,
		"benchmark_id": req.BenchmarkID,
		"attribution":  result,
	})
}

// --- ex-post risk ------------------------------------------------------------

type riskRequest struct {
	Portfolio      []float64 `json:"portfolio"`
	Benchmark      []float64 `json:"benchmark"`
	RiskFree       float64   `json:"risk_free_per_period"`
	PeriodsPerYear float64   `json:"periods_per_year"`
}

func (s *Server) handleRisk(w http.ResponseWriter, r *http.Request) {
	var req riskRequest
	if !decode(w, r, &req) {
		return
	}
	if req.PeriodsPerYear <= 0 {
		req.PeriodsPerYear = 252 // daily default
	}
	writeJSON(w, http.StatusOK, perf.ExPostRisk(req.Portfolio, req.Benchmark, req.RiskFree, req.PeriodsPerYear))
}

// --- helpers -----------------------------------------------------------------

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
