// Package server is the accounting (IBOR) service's HTTP surface (IBOR-01): it
// serves point-in-time NAV over the folded book-of-record and reconciles the book
// against a custodian statement. State lives in a ledger.Store (in-memory by
// default; a durable backend plugs in at the composition root). The bus consumer
// that folds OMS-01 fills + cash/corporate-action events into the journal is wired
// at the composition root behind the Store seam; the read endpoints here
// materialize the current-knowledge book and value it.
package server

import (
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"sync/atomic"
	"time"

	accounting "github.com/kanz-eng/kanz/services/accounting/internal"
	"github.com/kanz-eng/kanz/services/accounting/internal/ledger"
	"github.com/kanz-eng/kanz/services/accounting/internal/recon"
)

// Readiness gates traffic; the read endpoints are pure over the store, so the
// service is ready as soon as it is up (the flag exists for graceful shutdown).
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// Server is the HTTP handler.
type Server struct {
	logger        *slog.Logger
	readiness     *Readiness
	store         ledger.Store
	baseCcy       string
	metrics       http.Handler
	fxProvider    func() accounting.FXConverter
	instrumentCcy accounting.InstrumentCurrency
	mux           *http.ServeMux
}

// Option customizes the server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler (OBS-01a).
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

// WithLiveFX supplies a live FX provider (WIRE-01d): when a NAV request omits
// `fx`, the endpoint values the multi-currency book using the point-in-time
// converter this mints, instead of falling back to the domestic path. nil ⇒ no
// live FX (the pre-01d behavior).
func WithLiveFX(provider func() accounting.FXConverter) Option {
	return func(s *Server) { s.fxProvider = provider }
}

// WithInstrumentCurrency sets the default instrument→reference-currency map (the
// security-master join) used whenever a NAV request omits `instrument_currency`.
func WithInstrumentCurrency(m accounting.InstrumentCurrency) Option {
	return func(s *Server) { s.instrumentCcy = m }
}

// New builds the server over a journal store and base currency.
func New(readiness *Readiness, logger *slog.Logger, store ledger.Store, baseCcy string, opts ...Option) *Server {
	s := &Server{logger: logger, readiness: readiness, store: store, baseCcy: baseCcy, mux: http.NewServeMux()}
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
	s.mux.HandleFunc("POST /v1/portfolios/{id}/nav", s.handleNAV)
	s.mux.HandleFunc("POST /v1/portfolios/{id}/reconcile", s.handleReconcile)
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

// --- NAV ---------------------------------------------------------------------

type navRequest struct {
	Prices map[string]string `json:"prices"` // instrument -> price (exact decimal string)
	// FX and InstrumentCurrency enable the multi-currency valuation (PARITY-05f):
	// FX maps a currency to units-of-reporting-per-unit; InstrumentCurrency maps
	// an instrument to the currency its price is quoted in (default: the reporting
	// currency). When FX is present the NAV is valued via ComputeNAVInCurrency,
	// converting every foreign holding + cash bucket; absent ⇒ the domestic
	// single-currency path (unchanged). The composition root populates both from
	// the live FX feed + the MASTER security master; a client may also supply them.
	FX                 map[string]string `json:"fx,omitempty"`
	InstrumentCurrency map[string]string `json:"instrument_currency,omitempty"`
}

func (s *Server) handleNAV(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req navRequest
	if !decode(w, r, &req) {
		return
	}
	prices, err := toRatMap(req.Prices)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	book, err := ledger.MaterializeCurrent(r.Context(), s.store, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	nav, err := s.computeNAV(book, prices, req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"portfolio_id":   nav.PortfolioID,
		"currency":       nav.Currency,
		"total":          nav.Total.FloatString(2),
		"cash":           nav.Cash.FloatString(2),
		"security_value": nav.SecurityValue.FloatString(2),
		"accrued":        nav.Accrued.FloatString(2),
	})
}

// computeNAV values the book, choosing the FX source in precedence order: a
// request-supplied `fx` table (explicit client override) wins; else the live FX
// provider (WIRE-01d) values the multi-currency book with no `fx` in the
// request; else the domestic single-currency path. The instrument→currency join
// likewise prefers the request map, then the server default (the security
// master). ComputeNAVInCurrency stays completeness-gated — a foreign holding
// whose currency has no live rate yet fails the valuation loudly rather than
// mis-valuing.
func (s *Server) computeNAV(book *ledger.Book, prices map[string]*big.Rat, req navRequest) (accounting.NAV, error) {
	now := time.Now().UTC()

	var fx accounting.FXConverter
	switch {
	case len(req.FX) > 0:
		rates, err := toRatMap(req.FX)
		if err != nil {
			return accounting.NAV{}, err
		}
		fx = accounting.NewFXTable(s.baseCcy, rates)
	case s.fxProvider != nil:
		fx = s.fxProvider()
	default:
		return accounting.ComputeNAV(book, s.baseCcy, now, prices)
	}

	return accounting.ComputeNAVInCurrency(book, s.baseCcy, now, prices, s.instrumentCurrencyFor(req), fx)
}

// instrumentCurrencyFor picks the instrument→currency join for a request: the
// request-supplied map when present, else the server default.
func (s *Server) instrumentCurrencyFor(req navRequest) accounting.InstrumentCurrency {
	if len(req.InstrumentCurrency) > 0 {
		return accounting.InstrumentCurrency(req.InstrumentCurrency)
	}
	return s.instrumentCcy
}

// --- reconcile ---------------------------------------------------------------

type reconcileRequest struct {
	Positions map[string]string `json:"positions"` // instrument -> custodian quantity
	Cash      map[string]string `json:"cash"`      // currency -> custodian balance
	Tolerance string            `json:"tolerance"`
}

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req reconcileRequest
	if !decode(w, r, &req) {
		return
	}
	positions, err := toRatMap(req.Positions)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cash, err := toRatMap(req.Cash)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var tol *big.Rat
	if req.Tolerance != "" {
		if tol, err = parseRat(req.Tolerance); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	book, err := ledger.MaterializeCurrent(r.Context(), s.store, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	breaks := recon.Reconcile(book, recon.Statement{PortfolioID: id, Positions: positions, Cash: cash}, tol)
	out := make([]map[string]string, 0, len(breaks))
	for _, b := range breaks {
		out = append(out, map[string]string{
			"kind":      b.Kind.String(),
			"key":       b.Key,
			"ibor":      b.IBOR.FloatString(8),
			"custodian": b.Custodian.FloatString(8),
			"diff":      b.Diff.FloatString(8),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"breaks": out, "count": len(out)})
}

// --- helpers -----------------------------------------------------------------

func toRatMap(in map[string]string) (map[string]*big.Rat, error) {
	out := make(map[string]*big.Rat, len(in))
	for k, v := range in {
		r, err := parseRat(v)
		if err != nil {
			return nil, err
		}
		out[k] = r
	}
	return out, nil
}

func parseRat(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, &parseError{s}
	}
	return r, nil
}

type parseError struct{ s string }

func (e *parseError) Error() string { return "accounting: invalid decimal " + e.s }

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
