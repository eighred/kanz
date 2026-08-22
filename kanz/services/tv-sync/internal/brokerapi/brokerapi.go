// Package brokerapi serves tv-sync's TradingView Broker-Integration-API surface
// (REST + streaming) over the zero-truth projection. TradingView's Trading
// Terminal PULLS from this surface — accounts, positions, orders, executions,
// account state — and subscribes to the stream for live updates, so an
// Eighred trader watches the automated funds directly on TV charts.
//
// Every read is tenant-scoped: the tenant is taken from the trusted principal
// the edge injects (auth.HeaderPrincipalTenant; the api-gateway sets it from the
// authenticated principal, the MT-01 stance). An account that is not the
// caller's tenant simply is not found — cross-tenant reads are impossible, not
// merely denied.
//
// The resource model follows TradingView's Broker REST API; the exact field
// mapping is pinned against TV's spec document when the Trading Terminal is
// embedded (a thin adapter over these DTOs). The projection is bitemporal, so
// every list read accepts ?as_of=<RFC3339> to reconstruct "what we knew then".
package brokerapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/tv-sync/internal/projection"
)

// Handler serves the Broker-API endpoints over a projection.
type Handler struct {
	proj *projection.Projection
}

// New returns a Handler over the projection.
func New(proj *projection.Projection) *Handler { return &Handler{proj: proj} }

// Routes registers the Broker-API endpoints on a mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /broker/config", h.config)
	mux.HandleFunc("GET /broker/accounts", h.accounts)
	mux.HandleFunc("GET /broker/accounts/{id}/state", h.state)
	mux.HandleFunc("GET /broker/accounts/{id}/positions", h.positions)
	mux.HandleFunc("GET /broker/accounts/{id}/orders", h.orders)
	mux.HandleFunc("GET /broker/accounts/{id}/executions", h.executions)
	mux.HandleFunc("GET /broker/accounts/{id}/stream", h.stream)
}

// config advertises the broker's capabilities to the Trading Terminal. Read-only
// today: orders originate from strategies, not the chart (a later milestone may
// enable chart-initiated orders through the OMS command path).
func (h *Handler) config(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"supportsOrderModification": false,
		"supportsMarketOrders":      true,
		"supportsLimitOrders":       true,
		"supportsPositions":         true,
		"supportsExecutions":        true,
		"readOnly":                  true,
	})
}

func (h *Handler) accounts(w http.ResponseWriter, r *http.Request) {
	tenant, ok := auth.RequireCallerTenant(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": h.proj.Accounts(tenant)})
}

func (h *Handler) state(w http.ResponseWriter, r *http.Request) {
	tenant, asOf, ok := tenantAndAsOf(w, r)
	if !ok {
		return
	}
	st, found := h.proj.State(tenant, r.PathValue("id"), asOf)
	if !found {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *Handler) positions(w http.ResponseWriter, r *http.Request) {
	tenant, asOf, ok := tenantAndAsOf(w, r)
	if !ok {
		return
	}
	pos, found := h.proj.Positions(tenant, r.PathValue("id"), asOf)
	if !found {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"positions": pos})
}

func (h *Handler) orders(w http.ResponseWriter, r *http.Request) {
	tenant, asOf, ok := tenantAndAsOf(w, r)
	if !ok {
		return
	}
	ords, found := h.proj.Orders(tenant, r.PathValue("id"), asOf)
	if !found {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": ords})
}

func (h *Handler) executions(w http.ResponseWriter, r *http.Request) {
	tenant, asOf, ok := tenantAndAsOf(w, r)
	if !ok {
		return
	}
	execs, found := h.proj.Executions(tenant, r.PathValue("id"), r.URL.Query().Get("instrument"), asOf)
	if !found {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"executions": execs})
}

// stream is the Server-Sent-Events channel: the Trading Terminal subscribes and
// receives order/execution/position/state deltas as they fold. It is scoped to
// the caller's tenant + the account, so no cross-tenant update ever leaks.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	tenant, ok := auth.RequireCallerTenant(w, r)
	if !ok {
		return
	}
	accountID := r.PathValue("id")
	// The account must exist under this tenant — never open a stream to a
	// resource the caller cannot read.
	if _, found := h.proj.State(tenant, accountID, time.Time{}); !found {
		notFound(w)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// THIS IS THE ONE HANDLER IN THE ESTATE THAT LIFTS A SERVER TIMEOUT, AND
	// WITHOUT IT THIS ENDPOINT IS BROKEN RATHER THAN SLOW.
	//
	// Every server is built by internal/platform/httpserver, which sets
	// WriteTimeout (#235) — the bound that stops a wedged reader pinning a
	// goroutine and a file descriptor forever. But WriteTimeout bounds the WHOLE
	// response, and this response is unbounded by design: the Trading Terminal
	// holds it open for the length of a trading session. Under the standard
	// WriteTimeout this stream would be severed mid-session with no status, no
	// body and nothing logged — and the Terminal would show a book that had simply
	// stopped updating, which looks exactly like a quiet market.
	//
	// SetWriteDeadline(zero) clears it for THIS CONNECTION only; every other route
	// on this server stays bounded. That is why the answer is not "leave
	// WriteTimeout off tv-sync" — /broker/accounts and the rest are ordinary
	// buffered reads and must keep the bound.
	//
	// Only the WRITE deadline. ReadTimeout does not reach a running handler (it
	// bounds the request read; a handler that outlives it keeps a live context),
	// measured in internal/platform/httpserver's tests, so clearing it here would
	// be cargo cult. Cancellation still works: r.Context() below is cancelled when
	// the client disconnects.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		// Refuse rather than open a stream that will be cut at WriteTimeout: a
		// stream that dies silently mid-session is worse than one that never
		// opened, because only the second one is reported.
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	deltas, cancel := h.proj.Subscribe(tenant, accountID)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case d, open := <-deltas:
			if !open {
				return
			}
			b, err := json.Marshal(d)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", d.Kind, b)
			flusher.Flush()
		}
	}
}

// --- tenant + as-of extraction ---

// THE TENANT OF EVERY READ HERE COMES FROM auth.RequireCallerTenant.
//
// # This service authenticates NOTHING, and that is only safe behind the gateway
//
// It trusts that header because the caller can only be the gateway: the gateway
// validates the token, and a NetworkPolicy is what stops anyone else reaching
// this port — the control pkg/auth/meshheader.go states for the whole estate.
//
// IT IS NOT "the mesh (mTLS, SVID-authorized)", WHICH IS WHAT THIS SAID (#626).
// tv-sync serves plain HTTP through httpserver.New; no listener here sets
// TLSConfig and the gateway dials http://tv-sync.kanz-services.svc:8091. The
// claim was doubly misleading on THIS file, because tv-sync is one of the three
// services test/arch/network_policy_coverage_test.go lists in
// tenantHeaderTrustingServices — its ARM 3c residual says a pod in
// kanz-observability can reach this very port and name any tenant, since
// /metrics and this API share it. One guard recorded the exposure while this
// comment told a reader mTLS had closed it.
//
// EXPOSE THIS SERVICE DIRECTLY TO THE INTERNET AND ANY CALLER CAN NAME ANY
// TENANT AND READ THAT TENANT'S BOOK — its positions, its orders, its
// executions. There is no Ingress for tv-sync, deliberately; it is
// reachable only through /v1/broker/* on the gateway, which requires a principal.
//
// It used to read a bespoke "X-Tenant". Nothing set it, and any caller could. It
// then declared the platform header itself — as did audit, wealth, datamaster and
// the gateway, five copies of one concept (#258). The name and the refusal now
// live in pkg/auth, and the refusal is byte for byte the one this file wrote.

func tenantAndAsOf(w http.ResponseWriter, r *http.Request) (string, time.Time, bool) {
	tenant, ok := auth.RequireCallerTenant(w, r)
	if !ok {
		return "", time.Time{}, false
	}
	var asOf time.Time
	if v := r.URL.Query().Get("as_of"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid as_of: want RFC3339"})
			return "", time.Time{}, false
		}
		asOf = t
	}
	return tenant, asOf, true
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "account not found"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
