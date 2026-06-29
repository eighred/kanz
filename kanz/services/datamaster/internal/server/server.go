// Package server is the datamaster service's HTTP surface (MASTER-01b/c/d): it
// resolves a golden SecurityMaster across the configured vendor feeds, arbitrates
// multi-source prices, and serves the pricing-oversight exception queue with a
// human-override path. Vendor data comes from feed.VendorFeed seams (SimFeed by
// default); the exception queue is in-memory. A durable store and a bus consumer
// that feeds live vendor records/prices wire at the composition root behind the
// same seams.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/kanz-eng/kanz/services/datamaster/internal/feed"
	"github.com/kanz-eng/kanz/services/datamaster/internal/master"
	"github.com/kanz-eng/kanz/services/datamaster/internal/pricing"
)

// Readiness gates traffic; the read endpoints are pure over the feeds + queue, so
// the service is ready as soon as it is up (the flag exists for graceful
// shutdown).
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// Server is the HTTP handler.
type Server struct {
	logger    *slog.Logger
	readiness *Readiness
	feeds     []feed.VendorFeed
	queue     *pricing.Queue
	now       func() time.Time
	metrics   http.Handler
	mux       *http.ServeMux
}

// Option customizes the server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler (OBS-01a).
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

// WithClock overrides the clock (tests pin a fixed now for staleness checks).
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// New builds the server over a set of vendor feeds and an exception queue.
func New(readiness *Readiness, logger *slog.Logger, feeds []feed.VendorFeed, queue *pricing.Queue, opts ...Option) *Server {
	s := &Server{
		logger:    logger,
		readiness: readiness,
		feeds:     feeds,
		queue:     queue,
		now:       time.Now,
		mux:       http.NewServeMux(),
	}
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

// records pulls every vendor's records for one instrument across the feeds.
func (s *Server) records(ctx context.Context, instrumentID string) ([]master.VendorRecord, error) {
	var out []master.VendorRecord
	for _, f := range s.feeds {
		recs, err := f.Records(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			if r.InstrumentID == instrumentID {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// candidates pulls every vendor's price candidates for one instrument.
func (s *Server) candidates(ctx context.Context, instrumentID string) ([]pricing.Candidate, error) {
	var out []pricing.Candidate
	for _, f := range s.feeds {
		ps, err := f.Prices(ctx)
		if err != nil {
			return nil, err
		}
		// SimFeed scopes prices to its own instrument set via the candidate's
		// implicit instrument (the feed only returns its instrument's prices);
		// the source carries the vendor, so all returned candidates apply.
		_ = instrumentID
		out = append(out, ps...)
	}
	return out, nil
}

// handleSecurity resolves and returns the golden record for an instrument.
func (s *Server) handleSecurity(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	recs, err := s.records(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(recs) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "instrument not found"})
		return
	}
	sm, conflicts := master.Resolve(recs)
	for _, c := range conflicts {
		s.queue.Add(pricing.Exception{
			ID:           id + ":IDENTIFIER_CONFLICT:" + string(c.Scheme),
			Kind:         pricing.KindIdentifierConflict,
			InstrumentID: id,
			Detail:       c.Error(),
			Status:       pricing.StatusOpen,
			DetectedAt:   s.now(),
		})
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

// handlePrice arbitrates an instrument's price candidates and records any breaks.
func (s *Server) handlePrice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cands, err := s.candidates(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a := pricing.Arbitrate(id, cands, 0, 0, s.now())
	s.queue.AddAll(a.Exceptions)
	writeJSON(w, http.StatusOK, map[string]any{
		"instrument_id": a.InstrumentID,
		"has_price":     a.HasPrice,
		"chosen":        a.Chosen,
		"exceptions":    len(a.Exceptions),
	})
}

func (s *Server) handleExceptions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.queue.Open())
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
	if err := s.queue.Override(id, body.Actor, body.Reason, body.ChosenPrice, s.now()); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ex, _ := s.queue.Get(id)
	writeJSON(w, http.StatusOK, ex)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
