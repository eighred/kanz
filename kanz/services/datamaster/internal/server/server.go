// Package server is the datamaster service's HTTP surface (MASTER-01b/c/d): it
// serves the golden SecurityMaster, arbitrates multi-source prices, and serves the
// pricing-oversight exception queue with a human-override path.
//
// # What is read from the store, and what is read through to the vendors
//
// The golden record is READ FROM THE STORE. The projector resolves it on a cycle
// and persists it; this surface only reads it back. It used to call master.Resolve
// over the live feeds inside the request, which meant N reads cost N vendor API
// calls, the answer could change between two reads of the same instrument, and the
// resolution was a side effect of somebody happening to ask.
//
// The price is still READ THROUGH to the vendors and arbitrated per request. A
// price is a point-in-time observation and the freshest one is the right one to
// serve; the golden record is slowly-changing reference data and the right thing
// to project. What the two now share is the type: every price on this surface —
// candidate, consensus, and the price a human chose — is an exact decimal
// (DATA-M8b), and every one of them crosses the wire as a decimal STRING.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/datamaster/internal/feed"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
	"github.com/eighred/kanz/services/datamaster/internal/store"
)

// Readiness gates traffic. The composition root holds it down until the first
// golden projection has run, so the service never answers "instrument not found"
// merely because it has not read the vendors yet.
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// Server is the HTTP handler.
type Server struct {
	logger     *slog.Logger
	readiness  *Readiness
	tenant     string
	golden     store.GoldenStore
	exceptions store.ExceptionStore
	feeds      []feed.VendorFeed
	now        func() time.Time
	metrics    http.Handler
	mux        *http.ServeMux
}

// Option customizes the server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler (OBS-01a).
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

// WithClock overrides the clock (tests pin a fixed now for staleness checks).
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// New builds the server over the golden store, the exception store, and the vendor
// feeds the price path arbitrates.
// tenant is the tenant THIS INSTANCE serves — a required positional parameter,
// not an Option, because it is the input to the only cross-tenant control on
// this surface and an Option can be forgotten. Same stance as pg.NewTenantPool.
func New(readiness *Readiness, logger *slog.Logger, tenant string, golden store.GoldenStore, exceptions store.ExceptionStore, feeds []feed.VendorFeed, opts ...Option) *Server {
	s := &Server{
		logger:     logger,
		readiness:  readiness,
		tenant:     tenant,
		golden:     golden,
		exceptions: exceptions,
		feeds:      feeds,
		now:        time.Now,
		mux:        http.NewServeMux(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.logger == nil {
		s.logger = slog.Default()
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
	s.mux.HandleFunc("GET /v1/securities/{id}", s.handleSecurity)
	s.mux.HandleFunc("GET /v1/prices/{id}", s.handlePrice)
	s.mux.HandleFunc("GET /v1/exceptions", s.handleExceptions)
	s.mux.HandleFunc("POST /v1/exceptions/{id}/override", s.handleOverride)
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

// candidates pulls every vendor's price candidates FOR ONE INSTRUMENT. The filter
// on InstrumentID is load-bearing: a feed returns its whole price list, so without
// it this instrument would be arbitrated against every other instrument's quotes.
func (s *Server) candidates(ctx context.Context, instrumentID string) ([]pricing.Candidate, error) {
	var out []pricing.Candidate
	for _, f := range s.feeds {
		ps, err := f.Prices(ctx)
		if err != nil {
			return nil, err
		}
		for _, c := range ps {
			if c.InstrumentID == instrumentID {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// notFoundBody is the ONE body this surface returns for "no such thing",
// whether the row does not exist or belongs to another tenant. The two must be
// indistinguishable — see auth.RequireCallerTenantIs.
const notFoundBody = "not found"

// callerOwnsThisInstance reports whether the authenticated caller may touch this
// instance's data, and writes the refusal if not.
//
// ALL FOUR ROUTES HERE WERE UNSCOPED (#222) — including the WRITE,
// POST /v1/exceptions/{id}/override. golden_records, exceptions and
// exception_overrides are all tenant-scoped tables with RLS, and every handler
// went straight from r.PathValue to the store without reading the tenant the
// gateway had already injected.
//
// The check itself, the header name, and the no-oracle 404 are
// auth.RequireCallerTenantIs (#258) — the same three lines lived in four
// services and the gateway. Upstreams may trust that header ONLY because a
// NetworkPolicy makes the gateway their sole reachable caller; see #232 for
// where that is not yet enforced.
func (s *Server) callerOwnsThisInstance(w http.ResponseWriter, r *http.Request) bool {
	return auth.RequireCallerTenantIs(w, r, s.tenant, notFoundBody)
}

// handleSecurity returns the projected golden record for an instrument.
func (s *Server) handleSecurity(w http.ResponseWriter, r *http.Request) {
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
	id := r.PathValue("id")
	sm, ok, err := s.golden.Get(r.Context(), id)
	if err != nil {
		s.logger.Error("golden read failed", "instrument_id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "golden store unavailable"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "instrument not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instrument_id": sm.InstrumentID,
		"asset_class":   sm.AssetClass,
		"currency_code": sm.CurrencyCode,
		"description":   sm.Description,
		"identifiers": map[string]string{
			"isin": sm.Identifiers.ISIN, "cusip": sm.Identifiers.CUSIP,
			"sedol": sm.Identifiers.SEDOL, "figi": sm.Identifiers.FIGI, "ric": sm.Identifiers.RIC,
		},
		"provenance": sm.Provenance,
	})
}

// handlePrice arbitrates an instrument's price candidates and files any breaks.
func (s *Server) handlePrice(w http.ResponseWriter, r *http.Request) {
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
	id := r.PathValue("id")
	cands, err := s.candidates(r.Context(), id)
	if err != nil {
		s.logger.Error("vendor price fetch failed", "instrument_id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "vendor feed unavailable"})
		return
	}
	a := pricing.Arbitrate(id, cands, nil, 0, s.now())
	// A break that cannot be recorded must not be reported as recorded: the queue
	// is the oversight surface a human works, and an exception that vanished on the
	// way to it is worse than a failed read.
	if err := s.exceptions.AddAll(r.Context(), a.Exceptions); err != nil {
		s.logger.Error("exception file failed", "instrument_id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "exception store unavailable"})
		return
	}
	// The consensus goes out as an exact decimal STRING, never a JSON number: this
	// is the mark the instrument is valued at, and a regulator or a reconciler must
	// not have to guess which double we meant.
	out := map[string]any{
		"instrument_id": a.InstrumentID,
		"has_price":     a.HasPrice,
		"exceptions":    len(a.Exceptions),
	}
	if a.HasPrice {
		out["chosen"] = dec.Str(a.Chosen)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleExceptions(w http.ResponseWriter, r *http.Request) {
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
	open, err := s.exceptions.Open(r.Context())
	if err != nil {
		s.logger.Error("exception queue read failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "exception store unavailable"})
		return
	}
	if open == nil {
		open = []pricing.Exception{}
	}
	writeJSON(w, http.StatusOK, open)
}

func (s *Server) handleOverride(w http.ResponseWriter, r *http.Request) {
	if !s.callerOwnsThisInstance(w, r) {
		return
	}
	id := r.PathValue("id")
	// chosen_price is an exact decimal STRING. A JSON number is an IEEE-754 double
	// by definition, so accepting one would round the price a human chose on its
	// way into an append-only compliance record — it decodes into a string field
	// and is refused outright, the same contract the regulatory filing API takes
	// with money.
	var body struct {
		Actor       string `json:"actor"`
		Reason      string `json:"reason"`
		ChosenPrice string `json:"chosen_price"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid body: chosen_price must be an exact decimal string (a JSON number is a float)",
		})
		return
	}
	price, err := dec.ParseRat(body.ChosenPrice)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// And it must be a price this platform can actually carry. common.v1.Decimal
	// has a fixed scale, so a figure with more precision than that would be stored
	// exactly here and then rounded by every surface that shows or publishes it —
	// an audit record whose value nobody would ever see. Refuse it rather than keep
	// a number that disagrees with itself.
	//
	// THIS IS AN ASSERTION, NOT A CONVERSION — DO NOT "FIX" IT TO ToProtoScaled
	// (#94/#189). The fixed-scale round trip IS the question being asked: does this
	// operator-chosen price survive the platform's scale exactly? ToProtoScaled
	// raises the exponent instead of failing, so an over-precise price would round
	// to something representable and PASS — the precise input this check exists to
	// refuse. The sweep that moved the capital paths off the wrapping ToProto left
	// this site alone deliberately; changing it deletes a validation while looking
	// like consistency.
	//
	// It is fail-closed against the wrap too, incidentally: a value large enough to
	// wrap fails this comparison and is refused.
	if dec.FromProto(dec.ToProto(price)).Cmp(price) != 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "chosen_price carries more precision than the platform's decimal scale and cannot be represented exactly",
		})
		return
	}
	if err := s.exceptions.Override(r.Context(), id, body.Actor, body.Reason, price, s.now()); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ex, ok, err := s.exceptions.Get(r.Context(), id)
	if err != nil || !ok {
		s.logger.Error("override recorded but read-back failed", "exception_id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "exception store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, ex)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
