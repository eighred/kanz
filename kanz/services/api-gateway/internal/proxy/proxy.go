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
	"time"

	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// EVERY PROXIED CALL CARRIES A DEADLINE, AND THAT DEADLINE IS ITS ONLY BOUND.
//
// Forward used to inherit r.Context() unchanged, and an inbound request context carries
// NO deadline: net/http cancels it when the CLIENT disconnects, never when the UPSTREAM
// stops answering. So a wealth or datamaster pod that wedged mid-response — a GC pause, a
// blocked connection-pool acquire — and stopped writing WITHOUT closing the socket held a
// gateway goroutine and a file descriptor for as long as it stayed wedged.
// kanz_gateway_inflight_requests then climbs to the fd ceiling and the gateway, which is
// the sole ingress for ORDERS, stops accepting connections at all: a hung read surface
// takes down the write path.
//
// THE BOUND IS ON THE CALL, NOT ON THE CLIENT. http.Client.Timeout is enforced
// independently of the request context, so a client-level bound is a second limit on the
// same call and the effective one is min(context, client) — the smaller winning silently,
// with a message ("Client.Timeout exceeded while awaiting headers") that blames the
// network while the upstream may be fine. That failure has already been paid for once in
// the TUI; test/arch/probe_deadline_nesting_test.go is the standing rule, and it now
// covers this gateway's proxy client too.
//
// WHY THE VALUES ARE WHAT THEY ARE. A deadline shorter than a legitimate slow read turns a
// working system into a failing one, so neither of these is a round number picked for
// tidiness:
//
//   - readForwardTimeout is not a NEW bound. Since SVCWIRE-01c the mTLS proxy client has
//     carried Timeout: 30s, so on the production path 30s has been the live limit on these
//     reads all along. Moving it onto the call changes no request that works today; it
//     extends the same limit to the plaintext/dev path, which had none, and makes it a
//     limit an upstream error can be attributed to.
//   - copilotForwardTimeout is far longer because /v1/ask is not a read: the copilot runs
//     a tool-use turn against a model, and its own completion budget (openRouterTimeout,
//     services/copilot/cmd/copilot/model_openrouter.go) is FIVE MINUTES. The gateway
//     cannot honour that and does not pretend to — the public edge cuts at 120s
//     (proxy-read-timeout, infra/deploy/api-gateway-ingress.yaml), so no answer past two
//     minutes has ever been deliverable through here regardless of what this const says.
//     90s is the largest budget that still nests strictly inside the gateway's own
//     WriteTimeout and inside that edge bound, so a slow ask fails with the gateway's 502
//     and a log line naming the upstream, rather than nginx's anonymous 504. Closing the
//     remaining gap to the copilot's five minutes needs streaming (this proxy buffers with
//     io.ReadAll) and a looser edge — a different change, not a bigger number here.
const (
	readForwardTimeout    = 30 * time.Second
	copilotForwardTimeout = 90 * time.Second
)

// forwardBudget is the deadline for one proxied call, chosen by upstream.
//
// Keyed on the Service CONSTANT rather than on a route pattern: renaming a route is then
// invisible to this decision, and a mistyped service is a compile error rather than a
// silent fallthrough. A Service added without a case here inherits the read budget, which
// is the conservative direction — a new surface is bounded from its first request instead
// of being unbounded until someone remembers.
func forwardBudget(svc Service) time.Duration {
	switch svc {
	case ServiceCopilot:
		return copilotForwardTimeout
	default:
		return readForwardTimeout
	}
}

// Service identifies the upstream a route targets. The Backend resolves it to a
// per-service mTLS client (SVCWIRE-01c).
type Service string

const (
	ServiceWealth       Service = "wealth"
	ServiceDataMaster   Service = "datamaster"
	ServiceCopilot      Service = "copilot"
	ServiceTVSync       Service = "tv-sync"
	ServiceOptimization Service = "optimization"
	ServiceAccounting   Service = "accounting"
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
	roles   Roles
}

// Roles names the deployment's holders of the capabilities that are OPTIONAL —
// the ones whose routes are registered only when somebody can actually hold them.
//
// A STRUCT RATHER THAN POSITIONAL STRINGS. There are two of these now and #410's
// remaining acts will add a third; New(backend, "", "compliance") and
// New(backend, "compliance", "") differ by one argument position, are both valid
// Go, and produce a silent capability outage of exactly the #535 kind. A field
// name cannot be transposed.
//
// ROLE STRINGS RATHER THAN BOOLS, so the composition root passes what config
// already holds and nothing has to restate the condition. This package does not
// authorize — authz.Mux does — but it is where the route is registered, and
// REGISTRATION is the decision: see Routes.
type Roles struct {
	// Fund is the deployment's funder (API_GATEWAY_FUND_ROLE). EMPTY ⇒ the
	// cash-movement route is not registered.
	Fund string
	// Approve is the deployment's approver (API_GATEWAY_APPROVE_ROLE). EMPTY ⇒ the
	// three override routes are not registered (#539).
	Approve string
}

// New returns a proxy handler over the backend. A nil backend disables the
// routes; an empty role in Roles leaves that role's routes unregistered (#535).
func New(backend Backend, roles Roles) *Handler {
	return &Handler{backend: backend, roles: roles}
}

// Routes registers the Phase-7 read endpoints. They are 1:1 with the upstream
// service routes, so no path rewriting is needed — the gateway path IS the
// upstream path.
func (h *Handler) Routes(mux *authz.Mux) {
	mux.Handle(authz.Read, "GET /v1/households/{id}", h.handle(ServiceWealth, false, nil))
	mux.Handle(authz.Read, "GET /v1/securities/{id}", h.handle(ServiceDataMaster, false, nil))
	mux.Handle(authz.Read, "GET /v1/prices/{id}", h.handle(ServiceDataMaster, false, nil))
	mux.Handle(authz.Read, "GET /v1/exceptions", h.handle(ServiceDataMaster, false, nil))
	// The copilot is never anonymous: require the principal at the edge. It ASKS about the
	// book, it does not move it — a read (SEC-M2).
	mux.Handle(authz.Read, "POST /v1/ask", h.handle(ServiceCopilot, true, nil))

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
	mux.Handle(authz.Read, "GET /v1/broker/accounts", h.handle(ServiceTVSync, true, stripV1))
	mux.Handle(authz.Read, "GET /v1/broker/accounts/{id}/state", h.handle(ServiceTVSync, true, stripV1))
	mux.Handle(authz.Read, "GET /v1/broker/accounts/{id}/positions", h.handle(ServiceTVSync, true, stripV1))
	mux.Handle(authz.Read, "GET /v1/broker/accounts/{id}/orders", h.handle(ServiceTVSync, true, stripV1))
	mux.Handle(authz.Read, "GET /v1/broker/accounts/{id}/executions", h.handle(ServiceTVSync, true, stripV1))

	// FUNDING A PORTFOLIO (#415). The one route on this gateway that moves the
	// FUND'S OWN capital rather than the market's.
	//
	// authz.Fund, NOT authz.Trade. The person who can move money is never the
	// person who trades it — the oldest segregation of duties in fund operations,
	// and the argument is written out on the capability itself. Reusing Trade
	// would grant every strategy operator the authority to book a redemption
	// against the IBOR.
	//
	// requirePrincipal is TRUE. accounting takes the tenant off the header and
	// serves the one tenant its RLS pool is pinned to, so an anonymous forward
	// would be a cash movement nobody signed. Even an auth-disabled dev gateway
	// will not forward one.
	//
	// SAFE TO EXPOSE ONLY BECAUSE accounting NOW CHECKS THE TENANT. Until the
	// commit before this one it read no principal at all — every route went from
	// r.PathValue straight to the store — so fronting it here would have turned a
	// pod-to-pod gap into an internet-reachable one. The ownership gate lives
	// upstream, exactly as it does for the wealth and datamaster routes above.
	//
	// REGISTERED ONLY WHEN A FUNDER IS NAMED (#535), AND THAT IS THE ONE ROUTE ON
	// THIS HANDLER THAT IS CONDITIONAL. authz.Fund is carried by no role unless
	// API_GATEWAY_FUND_ROLE names one, and a route whose capability nobody holds
	// answers 403 to every principal that exists — "you may not", when the truth is
	// "nobody may, in this deployment". That is indistinguishable from a control
	// working as intended, which is how it survived from #415 to #535 unnoticed.
	// Unregistered, the answer is 404: there is no cash-movement surface here, and
	// that is true. Same stance as the /v1/control routes at the composition root
	// and as identity's provisioning surface.
	//
	// NOT GATED ON THE BACKEND, deliberately — a configured funder with no
	// accounting upstream still gets 503 from h.handle like every other route here,
	// because "the surface is disabled" and "you are not the funder" are different
	// answers and both are better than a 403 nobody can act on.
	if h.roles.Fund != "" {
		mux.Handle(authz.Fund, "POST /v1/portfolios/{id}/cash-movements",
			h.handle(ServiceAccounting, true, nil))
	}

	// THE SECOND SIGNATURE, AND THE DOOR IT NEEDED (#539, #410 act one).
	//
	// datamaster has carried a complete maker-checker workflow since #495/#498:
	// propose an override, sign it as a different person, list what is pending. The
	// gateway routed none of it, and under network-policies.yaml the gateway is
	// datamaster's ONLY permitted caller — so the control was reachable by nobody.
	// An override could not be proposed at all, armed or unarmed, and #444's
	// forged-actor fix guarded a surface nothing could touch. These three routes
	// are that repair; without them the whole path is ceremony.
	//
	// requirePrincipal is TRUE on all three. datamaster takes the actor off
	// X-Kanz-Principal-Subject and refuses a body naming anyone else (#444), and
	// takes the tenant off the same header to decide whether the exception is even
	// visible. An anonymous forward is an override signed by nobody — the exact
	// defect #444 fixed, arriving by the route that fix assumed was closed.
	//
	// THE PENDING QUEUE IS NOT authz.Read, and that is a deliberate refusal of the
	// obvious filing. Listing what awaits a signature looks like a report, but it
	// names the proposer of every unsigned change to the marks the book is valued
	// at, and it is the working surface an approver acts from rather than a view of
	// settled state. A read token that can see the queue but not sign it is a
	// half-open control, and the half that leaks is the interesting one.
	//
	// FOUR-EYES IS NOT ENFORCED HERE. The approver must differ from the proposer;
	// datamaster compares authenticated subjects, normalised for case and
	// surrounding space, and owns that rule with the tests that prove it. Restating
	// it at the edge would be the second implementation this repository keeps
	// paying for — 17 services each had their own secret() and 15 were wrong.
	//
	// REGISTERED ONLY WHEN AN APPROVER IS NAMED, for the reason spelled out on the
	// funding route above: authz.Approve is carried by no role unless
	// API_GATEWAY_APPROVE_ROLE names one, and a route whose capability nobody holds
	// answers 403 to every principal that exists. 404 is the true answer, and it is
	// the one an operator can act on.
	if h.roles.Approve != "" {
		mux.Handle(authz.Approve, "POST /v1/exceptions/{id}/override",
			h.handle(ServiceDataMaster, true, nil))
		mux.Handle(authz.Approve, "POST /v1/exceptions/{id}/override/approve",
			h.handle(ServiceDataMaster, true, nil))
		mux.Handle(authz.Approve, "GET /v1/exceptions/pending-overrides",
			h.handle(ServiceDataMaster, true, nil))
	}

	// PORTFOLIO CONSTRUCTION (#409). The optimization service authenticates
	// NOBODY, and it takes the issuer of a materialized order from its request
	// body. Once auto-publish is enabled that issuer is what the audit trail
	// records as the person who moved the capital — so a caller-supplied string is
	// precisely what the AUTH-01c forged-issuer guard exists to prevent.
	//
	// Routing it here is what makes the issuer real: the gateway authenticates,
	// injects the principal, and the service takes the issuer from that and refuses
	// a body that tries to name its own. requirePrincipal is true on BOTH routes, so
	// even an auth-disabled dev gateway forwards nothing anonymous.
	//
	// PROPOSE IS A READ: it computes a what-if against numbers in the request and
	// moves nothing — the same reasoning that makes POST /v1/portfolios/{id}/scenario
	// a read. The HTTP verb is not the authority on effect.
	// THE PATH IS REWRITTEN, and not cosmetically. The upstream serves /v1/propose
	// and /v1/orders, and /v1/orders is ALREADY the OMS order write surface on this
	// gateway (internal/orders) — mounting the upstream path verbatim is a route
	// collision that panics the mux at startup. The prefix also carries the
	// feature's name: "basket" means the deploy-time portfolio↔exchange-account
	// binding in this repo, guarded by test/arch/basket_contract_test.go, so this
	// feature is Model Portfolios and must not borrow that word.
	toUpstream := func(p string) string { return strings.TrimPrefix(p, "/v1/model-portfolios") }
	mux.Handle(authz.Read, "POST /v1/model-portfolios/propose", h.handle(ServiceOptimization, true, toUpstream))

	// MATERIALIZE IS TRADE, UNCONDITIONALLY — and that is a deliberate choice about
	// what a capability means. This route emits the order commands for a proposal,
	// and in a deployment with auto-publish on it puts them on the bus. The
	// capability describes what the route DOES in its most permissive
	// configuration, never what one deployment's environment variable currently
	// allows: a route whose required capability changes with a config flag is one
	// nobody can reason about, and a read token must not be able to reach a surface
	// that in some deployment moves capital.
	mux.Handle(authz.Trade, "POST /v1/model-portfolios/orders", h.handle(ServiceOptimization, true, toUpstream))
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
		// Derived from r.Context(), not from Background: a client that hangs up must
		// still cancel the upstream call immediately rather than leaving it to run out
		// the budget. See forwardBudget for why the budget differs per upstream.
		ctx, cancel := context.WithTimeout(r.Context(), forwardBudget(svc))
		defer cancel()

		resp, err := h.backend.Forward(ctx, Request{
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
