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
// The price is still READ THROUGH to the vendors and arbitrated per request. That
// is not an oversight: there is no durable price store, and there must not be one
// until pricing stops representing a price as a float64 (pricing's own package
// comment concedes this — fine for a comparison statistic, not for a number a
// valuation is struck on). A NUMERIC column filled from a float64 would launder a
// rounded number into a durable, audited price. Prices stay live until that is
// fixed; the breaks they raise are durable already.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/kanz-eng/kanz/services/datamaster/internal/feed"
	"github.com/kanz-eng/kanz/services/datamaster/internal/pricing"
	"github.com/kanz-eng/kanz/services/datamaster/internal/store"
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
func New(readiness *Readiness, logger *slog.Logger, golden store.GoldenStore, exceptions store.ExceptionStore, feeds []feed.VendorFeed, opts ...Option) *Server {
	s := &Server{
		logger:     logger,
		readiness:  readiness,
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

// handleSecurity returns the projected golden record for an instrument.
func (s *Server) handleSecurity(w http.ResponseWriter, r *http.Request) {
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
	id := r.PathValue("id")
	cands, err := s.candidates(r.Context(), id)
	if err != nil {
		s.logger.Error("vendor price fetch failed", "instrument_id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "vendor feed unavailable"})
		return
	}
	a := pricing.Arbitrate(id, cands, 0, 0, s.now())
	// A break that cannot be recorded must not be reported as recorded: the queue
	// is the oversight surface a human works, and an exception that vanished on the
	// way to it is worse than a failed read.
	if err := s.exceptions.AddAll(r.Context(), a.Exceptions); err != nil {
		s.logger.Error("exception file failed", "instrument_id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "exception store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instrument_id": a.InstrumentID,
		"has_price":     a.HasPrice,
		"chosen":        a.Chosen,
		"exceptions":    len(a.Exceptions),
	})
}

func (s *Server) handleExceptions(w http.ResponseWriter, r *http.Request) {
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
	id := r.PathValue("id")
	var body struct {
		Actor       string  `json:"actor"`
		Reason      string  `json:"reason"`
		ChosenPrice float64 `json:"chosen_price"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.exceptions.Override(r.Context(), id, body.Actor, body.Reason, body.ChosenPrice, s.now()); err != nil {
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
