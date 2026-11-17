// Package server is the regulatory filing service's HTTP surface (WIRE-01e): it
// assembles the delivered PARITY-06 filings — FRTB market-risk capital, Form PF,
// AIFMD leverage, and the TCFD/SFDR climate disclosures — from request-supplied
// inputs, records their original signature and separate context witness with
// durable FACTs, and serves them point-in-time.
//
// It owns no analytics: the existing File* assembler runs with the recorder's
// transaction-bound signer. Existing Report.Body owns rendered decimal values.
package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/filing"
	"github.com/eighred/kanz/internal/regulatory"
	regulatorypublish "github.com/eighred/kanz/internal/regulatory/publish"
	"github.com/eighred/kanz/internal/sustainability"
	sustainabilitypublish "github.com/eighred/kanz/internal/sustainability/publish"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	regulatorypb "github.com/eighred/kanz/kanz-schemas-go/regulatory/v1"
	sustainabilitypb "github.com/eighred/kanz/kanz-schemas-go/sustainability/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/regulatory/internal/recording"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Readiness gates traffic after the composition root has opened durable storage
// and its authenticated broker transport, and during graceful shutdown.
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// Server is the HTTP handler.
type Server struct {
	logger                  *slog.Logger
	readiness               *Readiness
	recorder                recording.Recorder
	tenant, producerVersion string
	metrics                 http.Handler
	mux                     *http.ServeMux
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

// WithRecorder binds one trusted tenant to one storage/broker account. There is
// no request-controlled credential/account selection and no signing-only fallback.
func WithRecorder(tenant, version string, recorder recording.Recorder) Option {
	return func(s *Server) { s.tenant, s.producerVersion, s.recorder = tenant, version, recorder }
}

// MetricsHandler returns the handler WithMetrics supplied, or nil. The
// composition root serves it on the metrics listener; nothing serves it here.
func (s *Server) MetricsHandler() http.Handler { return s.metrics }

// New builds the server. Filing routes refuse unless WithRecorder is supplied;
// metrics and the read-only ESG screen remain independently available.
func New(readiness *Readiness, logger *slog.Logger, opts ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}
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
	s.mux.HandleFunc("POST /v2/screening/esg", s.handleESGScreen)
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

// Filing requests require an explicit effective as_of and portfolio. Existing
// input structs remain the computation contract; the boundary additionally
// retains exact source-money tokens before the float climate analytic edge.
type frtbRequest struct {
	AsOf      *time.Time `json:"as_of"`
	Portfolio string     `json:"portfolio_id"`
	regulatory.FRTBInputs
}
type formPFRequest struct {
	AsOf      *time.Time `json:"as_of"`
	Portfolio string     `json:"portfolio_id"`
	regulatory.FormPFInputs
}
type aifmdRequest struct {
	AsOf      *time.Time `json:"as_of"`
	Portfolio string     `json:"portfolio_id"`
	regulatory.AIFMDInputs
}
type disclosureRequest struct {
	AsOf      *time.Time `json:"as_of"`
	Portfolio string     `json:"portfolio_id"`
	sustainability.DisclosureInputs
}

func (s *Server) handleFRTB(w http.ResponseWriter, r *http.Request) {
	if !s.filingCaller(w, r) {
		return
	}
	var req frtbRequest
	input, ok := readFilingInput(w, r, &req, nil, nil)
	if !ok {
		return
	}
	s.recordFiling(w, r, "FRTB", req.Portfolio, req.AsOf, input, nil, func(signer filing.Signer) (filing.Report, map[string]any, error) {
		rep, breakdown, err := regulatory.FileFRTB(req.FRTBInputs, *req.AsOf, signer)
		if err != nil {
			return rep, nil, err
		}
		out := rep.Body()
		out["breakdown"] = map[string]float64{"delta": breakdown.Delta, "vega": breakdown.Vega, "curvature": breakdown.Curvature, "drc": breakdown.DRC, "rrao": breakdown.RRAO, "total": breakdown.Total}
		return rep, out, nil
	})
}

func (s *Server) handleFormPF(w http.ResponseWriter, r *http.Request) {
	if !s.filingCaller(w, r) {
		return
	}
	var req formPFRequest
	input, ok := readFilingInput(w, r, &req, []string{"GrossAssetValue", "NetAssetValue", "VaR", "GrossExposure"}, nil)
	if !ok {
		return
	}
	s.recordFiling(w, r, "FORM_PF", req.Portfolio, req.AsOf, input, nil, func(signer filing.Signer) (filing.Report, map[string]any, error) {
		rep, err := regulatory.FileFormPF(req.FormPFInputs, *req.AsOf, signer)
		return rep, rep.Body(), err
	})
}

func (s *Server) handleAIFMD(w http.ResponseWriter, r *http.Request) {
	if !s.filingCaller(w, r) {
		return
	}
	var req aifmdRequest
	input, ok := readFilingInput(w, r, &req, []string{"NAV", "GrossExposure", "CommitmentExposure"}, nil)
	if !ok {
		return
	}
	s.recordFiling(w, r, "AIFMD", req.Portfolio, req.AsOf, input, nil, func(signer filing.Signer) (filing.Report, map[string]any, error) {
		rep, breakdown, err := regulatory.FileAIFMD(req.AIFMDInputs, *req.AsOf, signer)
		if err != nil {
			return rep, nil, err
		}
		out := rep.Body()
		out["breakdown"] = map[string]string{"aum": dec.Str(breakdown.AUM), "gross_leverage": dec.Str(breakdown.GrossLeverage), "commitment_leverage": dec.Str(breakdown.CommitmentLeverage)}
		return rep, out, nil
	})
}

func (s *Server) handleTCFD(w http.ResponseWriter, r *http.Request) {
	if !s.filingCaller(w, r) {
		return
	}
	var req disclosureRequest
	input, ok := readFilingInput(w, r, &req, nil, &req)
	if !ok {
		return
	}
	s.recordFiling(w, r, "TCFD", req.Portfolio, req.AsOf, input, &req, func(signer filing.Signer) (filing.Report, map[string]any, error) {
		rep, err := sustainability.FileTCFD(req.DisclosureInputs, *req.AsOf, signer)
		return rep, rep.Body(), err
	})
}

func (s *Server) handleSFDR(w http.ResponseWriter, r *http.Request) {
	if !s.filingCaller(w, r) {
		return
	}
	var req disclosureRequest
	input, ok := readFilingInput(w, r, &req, nil, &req)
	if !ok {
		return
	}
	s.recordFiling(w, r, "SFDR", req.Portfolio, req.AsOf, input, &req, func(signer filing.Signer) (filing.Report, map[string]any, error) {
		rep, err := sustainability.FileSFDR(req.DisclosureInputs, *req.AsOf, signer)
		return rep, rep.Body(), err
	})
}

func (s *Server) filingCaller(w http.ResponseWriter, r *http.Request) bool {
	principal, ok := auth.PrincipalFromHeaders(r.Header)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing authenticated filing principal"})
		return false
	}
	if !filing.BoundedIdentifier(s.tenant) || principal.Tenant != s.tenant {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "filing unavailable"})
		return false
	}
	if s.recorder == nil || s.producerVersion == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "filing recorder is not configured"})
		return false
	}
	return true
}

func (s *Server) recordFiling(w http.ResponseWriter, r *http.Request, framework, portfolio string, at *time.Time, input filingInput, climate *disclosureRequest, assemble func(filing.Signer) (filing.Report, map[string]any, error)) {
	principal, ok := auth.PrincipalFromHeaders(r.Header)
	if !ok || !filing.BoundedIdentifier(portfolio) || at == nil || at.IsZero() || !auth.PortfolioInScope(principal.Portfolios, portfolio) {
		badRequest(w, errors.New("explicit permitted portfolio and as_of required"))
		return
	}
	method := regulatorypublish.FilingMethod(framework)
	provenance := &domainpb.MeasureProvenance{Method: string(method), InputManifestDigest: input.digest, Params: map[string]string{"input_origin": "filing-request/unverified", "producer_version": s.producerVersion}}
	plan := recording.Plan{Tenant: s.tenant, Portfolio: portfolio, Framework: framework, AsOf: at.UTC(), InputDigest: input.digest}
	plan.Build = func(scope filing.PublicationScope, signer filing.Signer) (*regulatorypb.FilingRecordBody, map[string]any, error) {
		rep, out, err := assemble(signer)
		if err != nil {
			return nil, nil, err
		}
		wire, err := regulatorypublish.Report(rep, scope.ReportID, provenance)
		if err != nil {
			return nil, nil, err
		}
		body := &regulatorypb.FilingRecordBody{Report: wire, TenantId: scope.Tenant, PortfolioId: scope.Portfolio, InputOrigin: sustainabilitypb.ObservationOrigin_OBSERVATION_ORIGIN_FILING_REQUEST_UNVERIFIED, InputManifest: append([]byte(nil), input.manifest...), InputManifestDigest: input.digest, ProducerVersion: s.producerVersion, FilingCanonical: rep.Canonical()}
		body.RecordedAt = timestamppb.New(scope.RecordedAt)
		if climate != nil {
			body.InstrumentObservations, body.PortfolioObservation, err = sustainabilitypublish.Build(scope, input.holdings, func() (string, error) { id, err := uuid.NewV7(); return id.String(), err })
			if err != nil {
				return nil, nil, err
			}
			// The current climate model has both channels, with no separate kind
			// discriminator. Preserve that absence rather than invent a channel.
			if climate.Scenario.Name != "" || climate.Scenario.CarbonPrice != 0 || climate.Scenario.PhysicalSeverity != 0 {
				body.ClimateScenario = &sustainabilitypb.ClimateScenario{Name: climate.Scenario.Name, CarbonPrice: climate.Scenario.CarbonPrice, PhysicalSeverity: climate.Scenario.PhysicalSeverity}
			}
		}
		return body, out, nil
	}
	response, err := s.recorder.Record(r.Context(), plan)
	if err != nil {
		if errors.Is(err, recording.ErrUnpublished) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": recording.ErrUnpublished.Error()})
		} else if errors.Is(err, recording.ErrInvalidInput) {
			badRequest(w, err)
		} else {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "filing recording failed"})
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(response)
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

// maxRequestBytes bounds a /v1 request body (#769).
//
// IT IS A FLOOR, NOT THE BINDING CONSTRAINT, and the difference matters to
// anyone reading this number as permission. regulatory's /v1 surface is on
// :8103, which allow-gateway-to-regulatory admits from app: api-gateway ONLY,
// and the gateway already answers 413 above its own 1 MiB. So nothing that can
// reach this service today is anywhere near this cap.
//
// The point is that the bound stops being somebody else's. Every sibling holds
// its own — mcp 1 MiB, optimization 8 MiB "because an unbounded body drives
// O(n³) solver work" — and this service held none, so its only protection was a
// peer's configuration: gone if the NetworkPolicy widens, if a second caller is
// added, or if someone port-forwards for a debug session.
//
// 4 MiB is chosen against the largest thing a caller legitimately sends here: an
// FRTB or Form PF filing's line items, or the ESG screen's position array. Both
// are far below it.
const maxRequestBytes = 4 << 20 // 4 MiB

// decode reads a JSON request body under maxRequestBytes.
//
// TOO LARGE AND MALFORMED ARE DIFFERENT ANSWERS. 413 tells a caller to send
// less; 400 tells them to send it correctly, and a caller that cannot tell them
// apart retries the one thing that cannot work. json.Decode collapses both into
// "invalid request body" unless the limit error is checked for by type.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": "request body exceeds " + strconv.Itoa(maxRequestBytes) + " bytes",
			})
			return false
		}
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
