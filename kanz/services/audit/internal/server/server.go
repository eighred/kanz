package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/eighred/kanz/pkg/auth"
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

// THE TENANT OF EVERY READ ON THIS SURFACE COMES FROM auth.RequireCallerTenant,
// never from this package and never from the query string.
//
// This service authenticates NOTHING, and that is only safe behind the gateway.
// It is reachable only in-cluster over the SVID-authorized mesh, with no
// Ingress. EXPOSE IT DIRECTLY AND ANY CALLER READS EVERY TENANT'S AUDIT HISTORY
// — audit_log is deliberately not RLS'd (it is the cross-tenant compliance
// record), so the database will not save you here; this handler is the boundary.
//
// The header name and the refusal used to be declared here, and in three other
// services, and in the gateway (#258). They are now in pkg/auth so a change to
// how the platform's one identity header is validated is made once.

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
	tenant, ok := auth.RequireCallerTenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, ok := boundedLimit(q.Get("limit"), 100)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("limit must be between 1 and %d", report.MaxPageSize),
		})
		return
	}
	f := audit.Filter{
		Correlation: q.Get("correlation"),
		Tenant:      tenant,
		Kind:        audit.Kind(q.Get("kind")),
		EventType:   q.Get("event_type"),
		Limit:       limit,
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
	tenant, ok := auth.RequireCallerTenant(w, r)
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
	tenant, ok := auth.RequireCallerTenant(w, r)
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
	if _, ok := auth.RequireCallerTenant(w, r); !ok {
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
//
// THIS IS THE LONGEST HANDLER IN THE SERVICE AND THE EXPORT IS NOW PAGED (#304).
//
// It used to read a tenant's entire history: the "full-log" template carried an
// empty audit.Filter — no time window, no Limit — over a table that is WORM by
// contract and therefore only grows. #235 gave this server a WriteTimeout
// (internal/platform/httpserver, 120s) whose clock starts when the request is
// read, which converted an unbounded hang into a severed request with no status
// and no body. That was an improvement and NOT the fix: it made the defect
// visible at 120s rather than removing it, and the note here recorded that the
// real answer was a bounded report and not a larger number.
//
// That is what the code below now does. Every template ships a page size, ?limit=
// may adjust it within report.MaxPageSize, and ?after= resumes from the previous
// page's next_cursor (Seq — monotonic, assigned by Append, and unlike
// occurred_at it is unique, so a page boundary can neither skip nor repeat a
// record). The response states whether it is Complete, because the failure this
// endpoint had to avoid was never just OOM: a silently truncated audit export
// returns 200 OK and will be read as the tenant's whole history.
//
// STILL TRUE, AND STILL THE REASON NOT TO RAISE THE TIMEOUT: report.Generate
// attests the chain over the WHOLE log (Verify → store.Scan) on every page,
// because a page without its attestation is unattested evidence. That scan is
// unbounded by design — a hash chain cannot be verified from a suffix without a
// trusted anchor — so it, not the record read, is now the dominant cost here.
// Bounding it needs periodically signed checkpoints, which is a security design
// and not a query change; see the note on audit.Store.Scan.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	tenant, authed := auth.RequireCallerTenant(w, r)
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

	// PAGING (#304). The template ships a Limit; ?limit= may narrow or widen it
	// within MaxPageSize, and ?after= resumes from a previous page's
	// next_cursor. Both are safe to take from the caller in a way ?tenant= was
	// not: they select a WINDOW over records this principal is already entitled
	// to read, they cannot widen the tenant scope set above, and the resulting
	// read is bounded whatever is passed.
	q := r.URL.Query()
	limit, ok := boundedLimit(q.Get("limit"), tmpl.Filter.Limit)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("limit must be between 1 and %d", report.MaxPageSize),
		})
		return
	}
	tmpl.Filter.Limit = limit
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "after must be a non-negative sequence number from a previous page's next_cursor",
			})
			return
		}
		tmpl.Filter.AfterSeq = n
	}

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
	tenant, authed := auth.RequireCallerTenant(w, r)
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

// boundedLimit parses ?limit= for a read that MUST stay bounded, returning false
// for anything it will not serve.
//
// IT REJECTS ZERO, AND THAT IS THE BUG IT EXISTS TO CLOSE. It replaces an
// atoiOr(s, def) that accepted any n >= 0 — and audit.Filter.Limit == 0 does not
// mean "no rows", it means NO LIMIT CLAUSE (postgres.go's `if f.Limit > 0`). So
// `?limit=0` was a caller-supplied switch that turned this capped read into the
// tenant's entire append-only log: the same unbounded export as #304's full-log
// template, reachable on this route without any template at all. atoiOr was
// deleted rather than left beside this — a lenient parser sitting next to a
// strict one is how the lenient one gets picked for the next param.
//
// It also refuses ABOVE the cap rather than clamping. Clamping would answer a
// request for 10× the ceiling with a short page and no indication the number was
// ignored, which on an evidence export is the truncation-that-looks-complete
// this work exists to prevent. A refusal is unambiguous.
func boundedLimit(s string, def int) (int, bool) {
	if s == "" {
		return def, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > report.MaxPageSize {
		return 0, false
	}
	return n, true
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
