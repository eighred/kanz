// Package server is the regulatory filing service's HTTP surface (WIRE-01e): it
// assembles the delivered PARITY-06 filings — FRTB market-risk capital, Form PF,
// AIFMD leverage, and the TCFD/SFDR climate disclosures — from request-supplied
// inputs, signs them through the injected Signer, and serves them point-in-time.
//
// It owns no analytics: each endpoint decodes the delivered *Inputs struct
// directly (no DTO duplication), calls the delivered File* assembler (which is
// completeness-gated and validates its inputs — an incomplete or invalid filing
// is never signed), and renders the signed Report as JSON. This is the
// NAV-endpoint request→compute→sign→JSON pattern applied to regulatory filings.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/regulatory"
	"github.com/eighred/kanz/internal/sustainability"
)

// Signer signs a filing's canonical bytes. It is internal/filing's one-method
// seam — which regulatory.Signer and sustainability.Signer now alias — so a
// single value (the AUDIT-01 signer.ChainSigner, or a bare content-hash signer)
// drives every framework's filing. The composition root injects the concrete
// signer.
type Signer interface {
	// Sign returns the report's signature — its position in the durable audit
	// chain. An error means the chain link did NOT land, and the report must not be
	// issued: a filing carrying a chain position that exists in no chain is worse
	// than no filing.
	Sign(canonical []byte) (string, error)
}

// Readiness gates traffic; the endpoints are pure over the request, so the
// service is ready as soon as it is up (the flag exists for graceful shutdown).
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// Server is the HTTP handler.
type Server struct {
	logger    *slog.Logger
	readiness *Readiness
	signer    Signer
	metrics   http.Handler
	mux       *http.ServeMux
	// classifier resolves SECTOR and ISSUER for the ESG screen (#751).
	//
	// NIL IS A POSTURE, NOT A BUG. Without it those dimensions are UNRESOLVABLE,
	// and the shared COMP-01 engine REFUSES an unresolvable dimension rather than
	// passing it (#640) — so a screen against a sector exclusion answers "cannot
	// be verified" with the cause named. That is the honest answer for a
	// deployment with no reference-data source, and it is why the route is worth
	// mounting before one exists.
	classifier compliance.Classifier
}

// Option customizes the server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler (OBS-01a).
//
// IT IS NO LONGER MOUNTED ON THIS MUX (#765). /metrics now belongs to the
// SEPARATE metrics listener the composition root runs, because this mux carries
// the filing routes and a filing appends to the AUDIT-01 hash chain: serving
// both on one port put a write to the compliance record on the one
// allow-observability-scrape admits across every pod in the namespace.
//
// The option is kept and deliberately does nothing to the route table, so a
// caller that passes it does not silently re-open the port. MetricsHandler
// returns it for the composition root to serve on the other listener.
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

// WithClassifier supplies the instrument classifier the ESG screen resolves
// SECTOR and ISSUER through (#751). The composition root builds it from the
// reference-data cache; omitting it leaves those dimensions unresolvable, which
// the engine refuses rather than passes.
func WithClassifier(c compliance.Classifier) Option { return func(s *Server) { s.classifier = c } }

// MetricsHandler returns the handler WithMetrics supplied, or nil. The
// composition root serves it on the metrics listener; nothing serves it here.
func (s *Server) MetricsHandler() http.Handler { return s.metrics }

// New builds the server over an injected filing signer.
func New(readiness *Readiness, logger *slog.Logger, signer Signer, opts ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{logger: logger, readiness: readiness, signer: signer, mux: http.NewServeMux()}
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
	// NO /metrics HERE. See WithMetrics: this mux serves the filing routes, and
	// they write to the compliance chain.
	s.mux.HandleFunc("POST /v1/filings/frtb", s.handleFRTB)
	s.mux.HandleFunc("POST /v1/filings/formpf", s.handleFormPF)
	s.mux.HandleFunc("POST /v1/filings/aifmd", s.handleAIFMD)
	s.mux.HandleFunc("POST /v1/filings/tcfd", s.handleTCFD)
	s.mux.HandleFunc("POST /v1/filings/sfdr", s.handleSFDR)
	// THE ESG EXCLUSION SCREEN (#751 item 4). It is a READ — a pure function over
	// the book the caller supplies — and unlike the filing routes above it signs
	// nothing and appends nothing to the AUDIT-01 chain. See screen.go.
	s.mux.HandleFunc("POST /v1/screening/esg", s.handleESGScreen)
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

// --- filings -----------------------------------------------------------------

// asOfEnvelope carries the optional point-in-time as-of alongside the embedded
// filing inputs. The delivered *Inputs struct embeds into it, so its exported
// fields promote to top-level JSON keys — the request body is the delivered
// input set plus an `as_of`, with no duplicated DTO.
type frtbRequest struct {
	AsOf *time.Time `json:"as_of"`
	regulatory.FRTBInputs
}
type formPFRequest struct {
	AsOf *time.Time `json:"as_of"`
	regulatory.FormPFInputs
}
type aifmdRequest struct {
	AsOf *time.Time `json:"as_of"`
	regulatory.AIFMDInputs
}
type disclosureRequest struct {
	AsOf *time.Time `json:"as_of"`
	sustainability.DisclosureInputs
}

func (s *Server) handleFRTB(w http.ResponseWriter, r *http.Request) {
	var req frtbRequest
	if !decode(w, r, &req) {
		return
	}
	rep, breakdown, err := regulatory.FileFRTB(req.FRTBInputs, asOf(req.AsOf), s.signer)
	if err != nil {
		badRequest(w, err)
		return
	}
	out := rep.Body()
	out["breakdown"] = map[string]float64{
		"delta": breakdown.Delta, "vega": breakdown.Vega, "curvature": breakdown.Curvature,
		"drc": breakdown.DRC, "rrao": breakdown.RRAO, "total": breakdown.Total,
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleFormPF(w http.ResponseWriter, r *http.Request) {
	var req formPFRequest
	if !decode(w, r, &req) {
		return
	}
	rep, err := regulatory.FileFormPF(req.FormPFInputs, asOf(req.AsOf), s.signer)
	if err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep.Body())
}

func (s *Server) handleAIFMD(w http.ResponseWriter, r *http.Request) {
	var req aifmdRequest
	if !decode(w, r, &req) {
		return
	}
	rep, breakdown, err := regulatory.FileAIFMD(req.AIFMDInputs, asOf(req.AsOf), s.signer)
	if err != nil {
		badRequest(w, err)
		return
	}
	out := rep.Body()
	// Exact decimal strings, like the line items. The leverage ratios are a
	// compliance threshold — they must not round on the way out.
	out["breakdown"] = map[string]string{
		"aum": dec.Str(breakdown.AUM), "gross_leverage": dec.Str(breakdown.GrossLeverage),
		"commitment_leverage": dec.Str(breakdown.CommitmentLeverage),
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTCFD(w http.ResponseWriter, r *http.Request) {
	var req disclosureRequest
	if !decode(w, r, &req) {
		return
	}
	rep, err := sustainability.FileTCFD(req.DisclosureInputs, asOf(req.AsOf), s.signer)
	if err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep.Body())
}

func (s *Server) handleSFDR(w http.ResponseWriter, r *http.Request) {
	var req disclosureRequest
	if !decode(w, r, &req) {
		return
	}
	rep, err := sustainability.FileSFDR(req.DisclosureInputs, asOf(req.AsOf), s.signer)
	if err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep.Body())
}

// --- rendering ---------------------------------------------------------------
//
// There is no renderer here. internal/filing.Report.Body is THE filing body, and
// it puts the []filing.LineItem into the map so encoding/json calls
// LineItem.MarshalJSON — the same dec.Str rendering Report.Canonical signs.
//
// This file used to hold TWO hand-rolled renderers, regReport and climateReport,
// and they had diverged: the climate one passed the *big.Rat straight to
// encoding/json, whose TextMarshaler for big.Rat is RatString(). A WACI of
// 1234.5678 was served as "5429686605511341/4398046511104" while the signature in
// the same response committed to "1234.5678", so nothing the recipient could do
// with the body reproduced the signature it carried. Both packages already had a
// LineItem.MarshalJSON that did it correctly and neither was ever called (#633).
// Do not add a third: give filing.Report.Body the extra keys instead, as the FRTB
// and AIFMD handlers above do.

// --- helpers -----------------------------------------------------------------

// asOf resolves the request as-of: the supplied time, or now (UTC) when absent —
// so a filing is always stamped point-in-time.
func asOf(t *time.Time) time.Time {
	if t != nil && !t.IsZero() {
		return t.UTC()
	}
	return time.Now().UTC()
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return false
	}
	return true
}

func badRequest(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
