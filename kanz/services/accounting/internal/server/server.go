// Package server is the accounting (IBOR) service's HTTP surface (IBOR-01): it
// serves point-in-time NAV over the folded book-of-record and reconciles the book
// against a custodian statement. State lives in a ledger.Store (in-memory by
// default; a durable backend plugs in at the composition root). The bus consumer
// that folds OMS-01 fills + cash/corporate-action events into the journal is wired
// at the composition root behind the Store seam; the read endpoints here
// materialize the current-knowledge book and value it.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	accounting "github.com/eighred/kanz/services/accounting/internal"
	"github.com/eighred/kanz/services/accounting/internal/cashmove"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/recon"
)

// Readiness gates traffic; the read endpoints are pure over the store, so the
// service is ready as soon as it is up (the flag exists for graceful shutdown).
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// CashPublisher emits a cash movement as an accounting.v1 FACT (WIRE-01f). The
// composition root injects the bus-backed cashmove.Publisher when a broker is
// configured; without it the cash-movement endpoint is not mounted.
type CashPublisher interface {
	Publish(ctx context.Context, m cashmove.CashMovement) error
}

// Server is the HTTP handler.
type Server struct {
	logger        *slog.Logger
	readiness     *Readiness
	store         ledger.Store
	baseCcy       string
	metrics       http.Handler
	fxProvider    func() accounting.FXConverter
	instrumentCcy accounting.InstrumentCurrency
	cashPublisher CashPublisher
	// snapshotMetrics counts unbounded materializations. nil is inert.
	snapshotMetrics *ledger.SnapshotMetrics
	// tenant is the ONE tenant this instance serves. main pins the RLS pool to it
	// (pg.NewTenantPool), so a caller from any other tenant has no business here
	// whatever the database holds.
	//
	// EMPTY SERVES NOTHING, deliberately: auth.RequireCallerTenantIs fails closed on
	// an unset instance tenant, so a deployment that forgets WithTenant refuses every
	// /v1 route instead of serving them to everyone.
	tenant string
	mux    *http.ServeMux
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

// WithSnapshotMetrics shares the checkpoint job's collectors with the read
// path, so a materialization that had to scan the whole journal increments
// kanz_accounting_ledger_full_scans_total. Absent ⇒ the reads are still
// bounded, but nothing counts the ones that were not.
func WithSnapshotMetrics(m *ledger.SnapshotMetrics) Option {
	return func(s *Server) { s.snapshotMetrics = m }
}

// WithCashPublisher wires the cash-movement FACT producer (WIRE-01f) and mounts
// the POST /v1/portfolios/{id}/cash-movements endpoint. Absent ⇒ the endpoint is
// not mounted (no broker configured).
// WithTenant pins the instance to the one tenant it serves — the same value main
// gives pg.NewTenantPool. Without it every /v1 route refuses (#415).
func WithTenant(t string) Option { return func(s *Server) { s.tenant = t } }

func WithCashPublisher(p CashPublisher) Option {
	return func(s *Server) { s.cashPublisher = p }
}

// New builds the server over a journal store and base currency.
func New(readiness *Readiness, logger *slog.Logger, store ledger.Store, baseCcy string, opts ...Option) *Server {
	if logger == nil {
		// The read path LOGS now (materialize warns on an unbounded fold), so a
		// nil logger is a nil-deref in a handler rather than the harmless
		// unused field it used to be. Default it rather than panic per request.
		logger = slog.Default()
	}
	s := &Server{logger: logger, readiness: readiness, store: store, baseCcy: baseCcy, mux: http.NewServeMux()}
	for _, opt := range opts {
		opt(s)
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// notFoundBody is the single body a tenant-scoped route returns for a caller who
// does not own this instance. 404 and not 403: distinguishing "not yours" from
// "not there" turns portfolio-id enumeration into a cross-tenant directory.
const notFoundBody = "portfolio not found"

// callerOwnsThisInstance is the tenant gate every /v1 route runs first (#415).
//
// THE BOOK OF RECORD HAD NO GATE AT ALL. Every route went from r.PathValue
// straight to the store, and these are WRITES: a subscription or redemption
// posted to the IBOR, a NAV, a break list. Unscoped, a caller from any other
// tenant could name any portfolio id and move cash in this instance's book —
// #222's hole, in the service that moves capital rather than the one that prices
// it.
//
// Probes are deliberately NOT behind this: kubelet carries no principal, and a
// readiness probe that 404s takes the pod out of service.
func (s *Server) callerOwnsThisInstance(w http.ResponseWriter, r *http.Request) bool {
	return auth.RequireCallerTenantIs(w, r, s.tenant, notFoundBody)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	if s.metrics != nil {
		s.mux.Handle("GET /metrics", s.metrics)
	}
	s.mux.HandleFunc("POST /v1/portfolios/{id}/nav", s.handleNAV)
	s.mux.HandleFunc("POST /v1/portfolios/{id}/reconcile", s.handleReconcile)
	if s.cashPublisher != nil {
		s.mux.HandleFunc("POST /v1/portfolios/{id}/cash-movements", s.handleCashMovement)
	}
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

// materialize folds the current-knowledge book for a portfolio and RECORDS
// whether it could be served from a checkpoint (#229).
//
// Both read endpoints go through here rather than calling
// ledger.MaterializeCurrent directly, because the reason a read was unbounded is
// the only warning this service gets that its snapshot job has stopped working.
// Before #229 there was no snapshot job at all and every request took the
// unbounded path in silence for the life of the deployment. A full scan is
// logged at Warn and counted; it is not an error — the answer is correct, it
// just cost the whole journal to produce.
func (s *Server) materialize(ctx context.Context, portfolioID string) (*ledger.Book, error) {
	book, reason, err := ledger.MaterializeCurrent(ctx, s.store, portfolioID)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		s.snapshotMetrics.ObserveFullScan(reason)
		s.logger.Warn("materialized a book by scanning the ENTIRE journal",
			"portfolio_id", portfolioID, "reason", reason)
	}
	return book, nil
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
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
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
	book, err := s.materialize(r.Context(), id)
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
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
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
	book, err := s.materialize(r.Context(), id)
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

// --- cash movements (WIRE-01f) -----------------------------------------------

type cashMovementRequest struct {
	MovementID string `json:"movement_id"`
	Kind       string `json:"kind"`      // subscription | redemption | fee
	Amount     string `json:"amount"`    // positive decimal magnitude
	Currency   string `json:"currency"`  // ISO 4217; default base currency
	Effective  string `json:"effective"` // optional RFC-3339
	SourceRef  string `json:"source_ref"`
	// VenueAccountID scopes the movement to an exchange account (#415): "PF1 holds
	// 100 of the USDT in okx-sub-1", rather than only "PF1 holds 100 USDT".
	//
	// OPTIONAL, AND OMITTING IT IS A STATEMENT. Migration 0003 reads '' as the
	// positive declaration that the entry touched no exchange account — true for an
	// investor subscription into the fund's own bank, false for a transfer that
	// funded okx-sub-1. It is never defaulted, because a guessed account posts cash
	// against collateral it never reached.
	VenueAccountID string `json:"venue_account_id"`
}

// handleCashMovement books a non-trade cash movement by EMITTING it as a FACT
// (not writing the store directly) — the event-sourced path: the FACT lands on
// the bus and the WIRE-01f consumer folds it into the journal, so a replay
// reproduces the book. Returns 202 Accepted (the fold is asynchronous).
func (s *Server) handleCashMovement(w http.ResponseWriter, r *http.Request) {
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
	id := r.PathValue("id")
	var req cashMovementRequest
	if !decode(w, r, &req) {
		return
	}
	kind, err := parseKind(req.Kind)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	amount, err := parseRat(req.Amount)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ccy := req.Currency
	if ccy == "" {
		ccy = s.baseCcy
	}
	var eff time.Time
	if req.Effective != "" {
		if eff, err = time.Parse(time.RFC3339, req.Effective); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid effective time"})
			return
		}
	}
	mv := cashmove.CashMovement{
		MovementID:     req.MovementID,
		PortfolioID:    id,
		Kind:           kind,
		Amount:         amount,
		Currency:       ccy,
		Effective:      eff,
		SourceRef:      req.SourceRef,
		VenueAccountID: req.VenueAccountID,
	}
	if err := s.cashPublisher.Publish(r.Context(), mv); err != nil {
		// A validation error is the client's (bad movement); anything else is a
		// publish failure (broker) — surface both, but a bad request is 400.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "movement_id": req.MovementID})
}

func parseKind(s string) (cashmove.Kind, error) {
	switch s {
	case "subscription":
		return cashmove.Subscription, nil
	case "redemption":
		return cashmove.Redemption, nil
	case "fee":
		return cashmove.Fee, nil
	default:
		return 0, fmt.Errorf("accounting: unknown cash movement kind %q (want subscription|redemption|fee)", s)
	}
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
