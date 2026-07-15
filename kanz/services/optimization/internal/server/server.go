// Package server is the optimization service's HTTP surface (OPT-01e): optimize
// target weights + build a rebalance proposal, and materialize an approved
// proposal into OMS-01 order commands. Stateless over internal/optimization +
// the bridge; the market inputs (covariance/expected returns/current weights)
// arrive in the request, so the service needs no broker or database to serve the
// optimize/propose path. A real deployment wires the bus publisher + pre-trade
// gate behind the bridge seams at the composition root.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/kanz-eng/kanz/internal/optimization"
	"github.com/kanz-eng/kanz/services/optimization/internal/bridge"
)

// maxRequestBytes bounds a /v1 request body. A dense covariance for a several-
// hundred-asset universe fits well under 8 MiB; the cap turns an unbounded body
// (which drives O(n³)/O(k³) solver work) into a 400.
const maxRequestBytes = 8 << 20 // 8 MiB

// Readiness gates traffic; the compute endpoints are pure, so the service is
// ready as soon as it is up (the flag exists for graceful shutdown).
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
	s.mux.HandleFunc("POST /v1/propose", s.handlePropose)
	s.mux.HandleFunc("POST /v1/orders", s.handleOrders)
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

// --- propose -----------------------------------------------------------------

type proposeRequest struct {
	PortfolioID     string                      `json:"portfolio_id"`
	Instruments     []string                    `json:"instruments"`
	ExpectedReturns []float64                   `json:"expected_returns"`
	Covariance      [][]float64                 `json:"covariance"`
	BlackLitterman  *blRequest                  `json:"black_litterman"`
	Objective       optimization.Objective      `json:"objective"`
	Constraints     *optimization.ConstraintSet `json:"constraints"`
	Current         map[string]float64          `json:"current_weights"`
	NAV             float64                     `json:"nav"`
	Prices          map[string]float64          `json:"prices"`
	Threshold       float64                     `json:"threshold"`
}

// blRequest is the optional Black-Litterman input on /v1/propose. Present ⇒ the
// handler computes the posterior μ and uses it as ExpectedReturns before Optimize.
type blRequest struct {
	MarketWeights []float64 `json:"market_weights"`
	RiskAversion  float64   `json:"risk_aversion"`
	Tau           float64   `json:"tau"`
	Views         []viewDTO `json:"views"`
}

// viewDTO is one Black-Litterman view: a picking row p (length n), its return q,
// and its variance omega (0 ⇒ the He-Litterman default for this view).
type viewDTO struct {
	P     []float64 `json:"p"`
	Q     float64   `json:"q"`
	Omega float64   `json:"omega"`
}

func (s *Server) handlePropose(w http.ResponseWriter, r *http.Request) {
	var req proposeRequest
	if !decode(w, r, &req) {
		return
	}
	in := optimization.MarketInputs{Instruments: req.Instruments, ExpectedReturns: req.ExpectedReturns, Covariance: req.Covariance}
	if req.BlackLitterman != nil {
		mu, err := blMu(req)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		in.ExpectedReturns = mu
	}
	res, err := optimization.Optimize(in, req.Objective, req.Constraints)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	proposal := optimization.Rebalance(req.PortfolioID, req.Current, res.Weights, req.NAV, req.Prices, req.Threshold, time.Now())
	proposal.Objective = req.Objective
	proposal.ExpectedReturn = res.ExpectedReturn
	proposal.ExpectedRisk = res.ExpectedRisk
	writeJSON(w, http.StatusOK, proposal)
}

// --- materialize to orders ---------------------------------------------------

type ordersRequest struct {
	Proposal optimization.RebalanceProposal `json:"proposal"`
	Issuer   string                         `json:"issuer"`
}

// orderDTO is the JSON-friendly projection of a SubmitOrder command (protojson
// for the full proto is heavier than this reporting surface needs).
type orderDTO struct {
	OrderID      string  `json:"order_id"`
	PortfolioID  string  `json:"portfolio_id"`
	InstrumentID string  `json:"instrument_id"`
	Side         string  `json:"side"`
	Quantity     float64 `json:"quantity"`
	Issuer       string  `json:"issuer"`
}

func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	var req ordersRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Issuer == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "issuer required (issuer-bound commands)"})
		return
	}
	cmds := bridge.ToOrders(req.Proposal, req.Issuer)
	out := make([]orderDTO, 0, len(cmds))
	for _, c := range cmds {
		q := c.GetQuantity()
		out = append(out, orderDTO{
			OrderID:      c.GetOrderId(),
			PortfolioID:  c.GetPortfolioId(),
			InstrumentID: c.GetInstrumentId(),
			Side:         c.GetSide().String(),
			Quantity:     float64(q.GetCoefficient()) * pow10(q.GetExponent()),
			Issuer:       c.GetMetadata().GetIssuer(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": out, "count": len(out)})
}

// --- helpers -----------------------------------------------------------------

// blMu assembles a BLInput from the request's black_litterman block and returns
// the posterior expected returns. A view omega of 0 stays 0 in the diagonal we
// pass, which BlackLitterman reads as "use the He-Litterman default for this view".
func blMu(req proposeRequest) ([]float64, error) {
	bl := req.BlackLitterman
	k := len(bl.Views)
	p := make([][]float64, k)
	q := make([]float64, k)
	omega := make([]float64, k)
	for i, v := range bl.Views {
		p[i] = v.P
		q[i] = v.Q
		omega[i] = v.Omega
	}
	return optimization.BlackLitterman(optimization.BLInput{
		Covariance:    req.Covariance,
		MarketWeights: bl.MarketWeights,
		RiskAversion:  bl.RiskAversion,
		Tau:           bl.Tau,
		P:             p,
		Q:             q,
		Omega:         omega,
	})
}

func pow10(exp int32) float64 {
	p := 1.0
	for i := int32(0); i < exp; i++ {
		p *= 10
	}
	for i := int32(0); i < -exp; i++ {
		p /= 10
	}
	return p
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
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
