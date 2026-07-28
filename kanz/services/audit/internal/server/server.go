package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/eighred/kanz/services/audit/internal/audit"
	"github.com/eighred/kanz/services/audit/internal/lineage"
	"github.com/eighred/kanz/services/audit/internal/report"
	"github.com/eighred/kanz/services/audit/internal/soc2"
)

// Readiness gates traffic: starts NOT ready, flips once the store is reachable
// and (when configured) the subscription is live; shutdown clears it.
type Readiness struct{ ready atomic.Bool }

func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }
func (r *Readiness) Ready() bool    { return r.ready.Load() }

// HeaderPrincipalTenant is the tenant of the AUTHENTICATED caller, injected by
// the api-gateway — the platform's sole identity authority — from the verified
// token. The same header every other service reads
// (proxy.HeaderPrincipalTenant, tv-sync/brokerapi), deliberately: there is
// exactly one thing on this platform that decides who you are.
//
// This service authenticates NOTHING, and that is only safe behind the gateway.
// It is reachable only in-cluster over the SVID-authorized mesh, with no
// Ingress. EXPOSE IT DIRECTLY AND ANY CALLER READS EVERY TENANT'S AUDIT HISTORY
// — audit_log is deliberately not RLS'd (it is the cross-tenant compliance
// record), so the database will not save you here; this handler is the boundary.
const HeaderPrincipalTenant = "X-Kanz-Principal-Tenant"

type Server struct {
	logger    *slog.Logger
	readiness *Readiness
	metrics   http.Handler
	store     audit.Store
	mux       *http.ServeMux
}

type Option func(*Server)

// WithMetrics mounts a Prometheus /metrics handler (OBS-01a).
func WithMetrics(h http.Handler) Option { return func(s *Server) { s.metrics = h } }

// WithStore mounts the read API (query / lineage / verify / reports) over the
// audit store. Omitted ⇒ only the probes are served.
func WithStore(st audit.Store) Option { return func(s *Server) { s.store = st } }

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
	if s.store != nil {
		s.mux.HandleFunc("GET /v1/audit/events", s.handleQuery)
		s.mux.HandleFunc("GET /v1/audit/events/{event_id}", s.handleGet)
		s.mux.HandleFunc("GET /v1/audit/lineage/{event_id}", s.handleLineage)
		s.mux.HandleFunc("GET /v1/audit/verify", s.handleVerify)
		s.mux.HandleFunc("GET /v1/audit/reports/{template}", s.handleReport)
		s.mux.HandleFunc("GET /v1/soc2/evidence", s.handleSOC2Evidence)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.readiness.Ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleQuery serves GET /v1/audit/events with filter query params.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	// The tenant comes from the authenticated principal, NEVER from the query
	// string. `?tenant=` was caller-supplied input into a log that is
	// deliberately not RLS'd, so naming another tenant read their entire audit
	// history. Refused before the store is reached: an empty filter here does
	// not mean "no records", it means EVERY tenant's records.
	tenant, ok := tenantOf(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := audit.Filter{
		Correlation: q.Get("correlation"),
		Tenant:      tenant,
		Kind:        audit.Kind(q.Get("kind")),
		EventType:   q.Get("event_type"),
		Limit:       atoiOr(q.Get("limit"), 100),
	}
	if t, ok := parseTime(q.Get("since")); ok {
		f.Since = t
	}
	if t, ok := parseTime(q.Get("until")); ok {
		f.Until = t
	}
	recs, err := s.store.Query(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(recs), "records": recs})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantOf(w, r)
	if !ok {
		return
	}
	rec, found, err := s.store.Get(r.Context(), r.PathValue("event_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// A record belonging to another tenant is NOT FOUND, not FORBIDDEN: this log
	// is not RLS'd, so the store answers for every tenant, and "forbidden" would
	// confirm the record exists — a disclosure in itself. The caller learns only
	// that they have no such record.
	if !found || rec.TenantID != tenant {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// handleLineage serves the AUDIT-01c causal reconstruction for an event.
func (s *Server) handleLineage(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantOf(w, r)
	if !ok {
		return
	}
	lin, err := lineage.Reconstruct(r.Context(), s.store, tenant, r.PathValue("event_id"))
	if errors.Is(err, lineage.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, lin)
}

// handleVerify serves the AUDIT-01b chain attestation. A broken chain returns
// 409 Conflict — tamper detected — so a monitor can alert on the status code.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	// Authenticated, but deliberately NOT tenant-scoped. The hash chain is ONE
	// sequence across every tenant, so verifying a per-tenant subset proves
	// nothing about the chain — it would break the tamper-evidence this
	// endpoint exists to provide. What is removed here is ANONYMOUS access.
	// Restricting it further, to an operator capability, is an open decision
	// and must not be guessed at by narrowing the scan.
	if _, ok := tenantOf(w, r); !ok {
		return
	}
	att, err := report.Verify(r.Context(), s.store)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	code := http.StatusOK
	if !att.Verified {
		code = http.StatusConflict
	}
	writeJSON(w, code, att)
}

// handleReport generates a built-in template; ?format=csv overrides the default.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	tenant, authed := tenantOf(w, r)
	if !authed {
		return
	}
	tmpl, ok := report.BuiltIns()[r.PathValue("template")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown template"})
		return
	}
	if f := r.URL.Query().Get("format"); f != "" {
		tmpl.Format = report.Format(f)
	}
	// Scope the template's filter to the caller. BuiltIns() returns a fresh map
	// and tmpl is a copy, so this cannot leak into another request. Without it
	// the report renders EVERY tenant's audit history into a downloadable
	// artifact — the widest disclosure on this surface, because it is the one
	// endpoint whose output is designed to leave the building.
	tmpl.Filter.Tenant = tenant
	rep, err := report.Generate(r.Context(), s.store, tmpl, time.Now)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	switch tmpl.Format {
	case report.FormatCSV:
		b, err := rep.RenderCSV()
		if err != nil {
			s.fail(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	default:
		b, err := rep.RenderJSON()
		if err != nil {
			s.fail(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	}
}

// handleSOC2Evidence serves the PARITY-06d continuous-evidence bundle for an
// audit window (?from=&to=, RFC-3339; to defaults to now). Returns 409 when a
// mapped control has insufficient evidence — a Type II exception a monitor alerts
// on, the same status-code-as-signal stance handleVerify uses for a broken chain.
func (s *Server) handleSOC2Evidence(w http.ResponseWriter, r *http.Request) {
	// Same boundary as the reports endpoint, and for the same reason: this
	// evidence is compiled from audit records and handed to an AUDITOR, so an
	// unscoped collection puts other tenants' history into a document that
	// leaves the building.
	tenant, authed := tenantOf(w, r)
	if !authed {
		return
	}
	q := r.URL.Query()
	var from, to time.Time
	if t, ok := parseTime(q.Get("from")); ok {
		from = t
	}
	if t, ok := parseTime(q.Get("to")); ok {
		to = t
	}
	rep, err := soc2.CollectFromStore(r.Context(), s.store, tenant, from, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	code := http.StatusOK
	if !rep.Satisfied {
		code = http.StatusConflict
	}
	writeJSON(w, code, rep)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.ErrorContext(r.Context(), "audit api error", "path", r.URL.Path, "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n
	}
	return def
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// tenantOf reads the tenant of the authenticated caller, rejecting an unscoped
// request (deny-by-default). Mirrors tv-sync/brokerapi.tenantOf — the same
// header, the same refusal, because this is the same boundary.
func tenantOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenant := r.Header.Get(HeaderPrincipalTenant)
	if tenant == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "missing tenant scope: this surface is reachable only through the api-gateway, " +
				"which injects the authenticated principal",
		})
		return "", false
	}
	return tenant, true
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
