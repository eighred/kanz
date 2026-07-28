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

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/regulatory"
	"github.com/eighred/kanz/internal/sustainability"
)

// Signer signs a filing's canonical bytes. It is the one-method seam both
// regulatory.Signer and sustainability.Signer expose, so a single value (the
// AUDIT-01 signer.ChainSigner, or a bare content-hash signer) drives every
// framework's filing. The composition root injects the concrete signer.
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
}

// Option customizes the server.
type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler (OBS-01a).
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

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
	if s.metrics != nil {
		s.mux.Handle("GET /metrics", s.metrics)
	}
	s.mux.HandleFunc("POST /v1/filings/frtb", s.handleFRTB)
	s.mux.HandleFunc("POST /v1/filings/formpf", s.handleFormPF)
	s.mux.HandleFunc("POST /v1/filings/aifmd", s.handleAIFMD)
	s.mux.HandleFunc("POST /v1/filings/tcfd", s.handleTCFD)
	s.mux.HandleFunc("POST /v1/filings/sfdr", s.handleSFDR)
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
	out := regReport(rep)
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
	writeJSON(w, http.StatusOK, regReport(rep))
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
	out := regReport(rep)
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
	writeJSON(w, http.StatusOK, climateReport(rep))
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
	writeJSON(w, http.StatusOK, climateReport(rep))
}

// --- rendering ---------------------------------------------------------------

// regReport renders a regulatory.Report (FRTB/FormPF/AIFMD) to the JSON body.
func regReport(rep regulatory.Report) map[string]any {
	items := make([]map[string]any, len(rep.LineItems))
	for i, li := range rep.LineItems {
		// The value is emitted as a decimal STRING, never a JSON number. A regulator
		// parsing our filing must not have to guess which IEEE-754 double we meant by
		// 0.1, and this is the same rendering the signature commits to.
		items[i] = map[string]any{"code": li.Code, "label": li.Label, "value": dec.Str(li.Value)}
	}
	return map[string]any{
		"framework":  string(rep.Framework),
		"as_of":      rep.AsOf.UTC().Format(time.RFC3339),
		"line_items": items,
		"signature":  rep.Signature,
	}
}

// climateReport renders a sustainability.Report (TCFD/SFDR) to the JSON body.
func climateReport(rep sustainability.Report) map[string]any {
	items := make([]map[string]any, len(rep.LineItems))
	for i, li := range rep.LineItems {
		items[i] = map[string]any{"code": li.Code, "label": li.Label, "value": li.Value}
	}
	return map[string]any{
		"framework":  string(rep.Framework),
		"as_of":      rep.AsOf.UTC().Format(time.RFC3339),
		"line_items": items,
		"signature":  rep.Signature,
	}
}

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
