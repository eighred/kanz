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

	// Maker-checker on the override path (#410). See dualcontrol.go.
	proposals          store.ProposalStore
	requireDualControl bool
	dualTTL            time.Duration
	overrideMetrics    *overrideMetrics
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
	// The second signature, and the list that makes an unapproved override
	// visible rather than silently dropped (#410).
	s.mux.HandleFunc("POST /v1/exceptions/{id}/override/approve", s.handleApproveOverride)
	s.mux.HandleFunc("GET /v1/exceptions/pending-overrides", s.handlePendingOverrides)
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
//
// # It withheld the classification it exists to master (#640)
//
// This body carried instrument_id, asset_class, currency_code, description and
// the identifiers — and STOPPED. The two fields a compliance control actually
// buckets on, sector and issuer_id, were resolved by master.Resolve, persisted
// in golden_records, and then dropped on the way out, so the only read path to
// the security master could not answer "what sector is this" at all. That is
// half of why every SECTOR/ISSUER mandate on this estate was unresolvable: the
// data existed one process away with no way to ask for it.
//
// SECTOR AND AS_OF ARE ALWAYS PRESENT, even when empty. An absent key and an
// empty value would be the same on the wire, and they are different facts — the
// master resolved no sector for this instrument, versus this endpoint does not
// serve sectors. refdata.Client keys its refusal on the first, so it must be
// able to see it.
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
	// RFC3339, and empty rather than "0001-01-01T00:00:00Z" for a record no
	// vendor dated: a zero timestamp rendered as a real one reads as a 1st-century
	// snapshot, and a point-in-time consumer would compare against it.
	asOf := ""
	if !sm.AsOf.IsZero() {
		asOf = sm.AsOf.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"instrument_id": sm.InstrumentID,
		"asset_class":   sm.AssetClass,
		"currency_code": sm.CurrencyCode,
		"description":   sm.Description,
		"issuer_id":     sm.IssuerID,
		"sector": map[string]string{
			"taxonomy": sm.Sector.Taxonomy, "code": sm.Sector.Code, "name": sm.Sector.Name,
		},
		"as_of": asOf,
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
	// WHO IS ASKING? The gateway is the sole identity authority on this platform:
	// it verifies the token and injects the principal headers, and this service is
	// reachable only through it (a NetworkPolicy is what makes trusting those
	// headers sound). No principal means either the caller bypassed the gateway or
	// the gateway is misconfigured — both are refusals.
	//
	// THIS SURFACE READ THOSE HEADERS FOR THE TENANT AND IGNORED THE IDENTITY
	// (#410). callerOwnsThisInstance above has checked X-Kanz-Principal-Tenant
	// since #222, while the ACTOR — "a named human's signed decision", written to
	// an append-only audit trail — came out of the JSON body. Any caller entitled
	// to override could therefore sign ANY NAME THEY TYPED into that trail,
	// including a colleague's, and nothing recorded the substitution.
	//
	// The tenant header establishes WHICH tenant is asking. It never establishes
	// WHO. An audit record whose actor is self-asserted is not an audit record.
	subject, ok := s.authenticatedSubject(w, r)
	if !ok {
		return
	}
	// chosen_price is an exact decimal STRING. A JSON number is an IEEE-754 double
	// by definition, so accepting one would round the price a human chose on its
	// way into an append-only compliance record — it decodes into a string field
	// and is refused outright, the same contract the regulatory filing API takes
	// with money.
	var body struct {
		// Actor is ACCEPTED BUT NEVER TRUSTED: it is compared to the authenticated
		// subject and must match. Kept in the contract so a client that echoes its
		// own subject keeps working, and so a client attributing the decision to
		// SOMEONE ELSE is told, rather than having its value silently replaced —
		// which would leave it believing it recorded a name the trail does not hold.
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
	// THE ACTOR IS THE AUTHENTICATED CALLER, NEVER THE BODY (#410). A body naming
	// someone else is refused rather than silently replaced: overwriting it would
	// leave the client believing it recorded Bob's decision while the trail says
	// Alice, with nothing anywhere reporting the disagreement. Same rule, same
	// reasoning, as the optimization service's forged-issuer refusal (AUTH-01c).
	if body.Actor != "" && body.Actor != subject {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "actor is taken from the authenticated principal and must not name anyone else; " +
				"an override signed into an append-only compliance record under another person's " +
				"name is the forged-issuer defect AUTH-01c prevents",
		})
		return
	}
	// MAKER-CHECKER (#410). Armed, this records a PENDING proposal and answers
	// 202 — it does NOT apply. Unarmed, the override applies on one person's
	// authority and is counted as single-signed, which is the gap this issue
	// exists to close and, until it is armed, to measure.
	//
	// The reason string is required by pricing.Override.Validate, and checking it
	// HERE means a proposal cannot be recorded that would fail only at approval
	// time — when the person who could fix it has gone.
	if body.Reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "override requires a reason"})
		return
	}
	if s.dualControlArmed() {
		s.proposeOverride(w, r, id, subject, body.Reason, price)
		return
	}
	s.applySingleSigned(w, r, id, subject, body.Reason, price)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
