// Package proxy is the api-gateway's read-surface forwarder for the Phase-7
// services (SVCWIRE-01b): it exposes the wealth / datamaster / copilot read
// routes through the gateway behind the SAME edge chain (version → signing →
// auth → metrics → quota → idempotency) as the risk and order surfaces, then
// forwards each request to its upstream service, propagating the authenticated
// principal as the identity the upstream trusts.
//
// This package owns the route shapes + the edge-auth contract — in particular,
// the copilot /v1/ask is NEVER anonymous (the principal is required at the edge,
// matching the copilot service's own rule). The forwarding TRANSPORT is a seam:
// a nil Backend disables the routes (they 503), exactly like the orders write
// surface with a nil publisher; the concrete mTLS HTTP client (the SEC-01b mesh
// stance) wires at the composition root in SVCWIRE-01c. So this layer is the
// edge; the Backend is the wire.
package proxy

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kanz-eng/kanz/services/api-gateway/internal/middleware"
)

// Service identifies the upstream a route targets. The Backend resolves it to a
// per-service mTLS client (SVCWIRE-01c).
type Service string

const (
	ServiceWealth     Service = "wealth"
	ServiceDataMaster Service = "datamaster"
	ServiceCopilot    Service = "copilot"
	ServiceTVSync     Service = "tv-sync"
)

// Request is the upstream call the Backend forwards. Principal is the
// authenticated caller the upstream trusts (the gateway is the sole identity
// authority — it propagates the principal over the mesh, the orders.go stance).
type Request struct {
	Service   Service
	Method    string
	Path      string
	Query     url.Values
	Body      []byte
	Principal *middleware.Principal
}

// Response is the upstream reply, written back to the client verbatim.
type Response struct {
	Status      int
	ContentType string
	Body        []byte
}

// Backend forwards a Request to its upstream service over the mesh. The concrete
// implementation is a per-service mTLS HTTP client (SVCWIRE-01c); it returns
// ErrBackendUnavailable when the targeted upstream is not wired. A nil Backend
// disables the proxy routes entirely (they 503).
type Backend interface {
	Forward(ctx context.Context, req Request) (Response, error)
}

// Handler serves the Phase-7 read routes. A nil backend means the proxy is
// disabled (the gateway runs without the Phase-7 surfaces) — the routes 503,
// mirroring the orders write surface.
type Handler struct {
	backend Backend
}

// New returns a proxy handler over the backend. A nil backend disables the
// routes.
func New(backend Backend) *Handler { return &Handler{backend: backend} }

// Routes registers the Phase-7 read endpoints. They are 1:1 with the upstream
// service routes, so no path rewriting is needed — the gateway path IS the
// upstream path.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/households/{id}", h.handle(ServiceWealth, false, nil))
	mux.HandleFunc("GET /v1/securities/{id}", h.handle(ServiceDataMaster, false, nil))
	mux.HandleFunc("GET /v1/prices/{id}", h.handle(ServiceDataMaster, false, nil))
	mux.HandleFunc("GET /v1/exceptions", h.handle(ServiceDataMaster, false, nil))
	// The copilot is never anonymous: require the principal at the edge.
	mux.HandleFunc("POST /v1/ask", h.handle(ServiceCopilot, true, nil))

	// THE TRADINGVIEW BROKER SURFACE — what a chart shows an authorized human about
	// the orders Kanz opened for them.
	//
	// tv-sync AUTHENTICATES NOTHING. It reads the tenant out of a header and trusts
	// it, because the gateway is the sole identity authority on this platform and the
	// mesh is what stops anyone else reaching the service. Put tv-sync on the open
	// internet as it stands and ANY CALLER COULD NAME ANY TENANT AND READ THAT
	// TENANT'S BOOK. So it is reachable only through here, and only with a principal:
	// requirePrincipal is true, so even an auth-disabled dev gateway will not forward
	// an anonymous request for somebody's positions.
	//
	// The path is rewritten (/v1/broker/... → /broker/...) because the gateway's edge
	// chain — version, signing, AUTH, metrics, quota, idempotency — is mounted on
	// /v1/. A route outside /v1 would skip every one of those. The prefix is not
	// cosmetic; it is what makes the request authenticated at all.
	//
	// NOT PROXIED: GET /broker/accounts/{id}/stream. It is Server-Sent Events, and
	// this backend BUFFERS the upstream response (io.ReadAll) — a stream through it
	// would hang until 8 MiB or forever, whichever came first. A live feed needs a
	// streaming reverse proxy, which this is deliberately not.
	stripV1 := func(p string) string { return strings.TrimPrefix(p, "/v1") }
	mux.HandleFunc("GET /v1/broker/accounts", h.handle(ServiceTVSync, true, stripV1))
	mux.HandleFunc("GET /v1/broker/accounts/{id}/state", h.handle(ServiceTVSync, true, stripV1))
	mux.HandleFunc("GET /v1/broker/accounts/{id}/positions", h.handle(ServiceTVSync, true, stripV1))
	mux.HandleFunc("GET /v1/broker/accounts/{id}/orders", h.handle(ServiceTVSync, true, stripV1))
	mux.HandleFunc("GET /v1/broker/accounts/{id}/executions", h.handle(ServiceTVSync, true, stripV1))
}

// handle builds a forwarding handler for one upstream. requirePrincipal gates
// the route on an authenticated caller at the edge (copilot's /v1/ask), beyond
// the chain's auth middleware — so even an auth-disabled dev gateway never
// forwards an anonymous question.
func (h *Handler) handle(svc Service, requirePrincipal bool, rewrite func(string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.backend == nil {
			writeError(w, http.StatusServiceUnavailable, "the "+string(svc)+" surface is disabled")
			return
		}
		p := middleware.PrincipalFromContext(r.Context())
		if requirePrincipal && (p == nil || p.Subject == "") {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "read body failed")
			return
		}
		if len(body) > maxBodyBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		path := r.URL.Path
		if rewrite != nil {
			path = rewrite(path)
		}
		resp, err := h.backend.Forward(r.Context(), Request{
			Service:   svc,
			Method:    r.Method,
			Path:      path,
			Query:     r.URL.Query(),
			Body:      body,
			Principal: p,
		})
		if err != nil {
			writeForwardError(w, err)
			return
		}
		writeUpstream(w, resp)
	}
}
