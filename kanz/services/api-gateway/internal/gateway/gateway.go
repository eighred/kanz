// Package gateway is the api-gateway BFF's REST↔gRPC transcoding layer
// (API-01c). It exposes a JSON/HTTP surface over the risk-engine's
// query.v1.RiskQueryService gRPC API: each REST route builds the proto
// request, calls the gRPC client, and marshals the proto response with
// protojson — so the REST and gRPC representations are the SAME messages
// (transcoding parity, API-01e), no hand-maintained DTOs.
//
// Why hand-rolled transcoding (not grpc-gateway codegen): the surface is four
// read RPCs, and a thin protojson adapter keeps the dependency footprint to
// the protobuf runtime already in the module — no google.api.http annotations,
// no generated .gw.go, no extra plugin in the schema pipeline. The OpenAPI doc
// (openapi.go) is emitted alongside so clients still get a machine-readable
// contract.
package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"math"
	"strconv"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/pkg/auth"
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// Handler serves the REST surface by forwarding to a RiskQueryServiceClient.
// It depends on the generated client INTERFACE, so tests inject a fake and the
// real gateway injects an mTLS-dialed client (main).
type Handler struct {
	// orders is the OMS read surface, or nil when this deployment fronts none.
	orders orderpb.OrderQueryServiceClient
	// approveRole is the deployment's approver (API_GATEWAY_APPROVE_ROLE). EMPTY ⇒
	// the pending-approvals queue is not registered, so the deployment answers 404
	// rather than 403 to everybody (#535).
	approveRole string
	// instruments is the OMS's tradeable-pair catalogue, or nil for the same
	// reason (#406). It rides the SAME OMS connection as orders — one upstream,
	// two contracts — so a deployment fronting an OMS has both or neither.
	instruments venuepb.VenueQueryServiceClient
	// risk resolves the risk-engine client for the CALLER'S TENANT. A tenant with
	// its own rendered engine (internal/tenantgen.Services) holds its own
	// positions, so answering it from the platform engine would return another
	// book's exposure and VaR as its own (#668). See riskclients.go.
	risk      *riskClients
	marshaler protojson.MarshalOptions
	// logger receives the detail of every 5xx. It is the ONLY place that detail
	// goes now: the client gets a constant, so if this is not wired the
	// information is not merely hidden from the caller, it is destroyed. Never
	// nil — New substitutes slog.Default() — because a fault this platform caused
	// and did not record is strictly worse than one it leaked.
	logger *slog.Logger
}

// New returns a Handler over the risk-engine client and, optionally, the OMS's
// order-history client.
//
// orders MAY BE NIL, and then the history route is not registered at all (#399).
// A deployment whose OMS serves no read surface should 404 that path rather than
// answer it with an error: an unregistered route says "not configured here",
// while a registered one that always fails says "broken", and only one of those
// is true. It is the same shape the control routes take for an absent operator.
//
// logger MAY BE NIL and then slog.Default() is used. That is a convenience for
// tests, not a licence for the composition root: main wires obs.Logger, and the
// detail of every 5xx now exists only in this logger's output.
func New(client querypb.RiskQueryServiceClient, orders orderpb.OrderQueryServiceClient, instruments venuepb.VenueQueryServiceClient, approveRole string, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		risk:        newRiskClients(client),
		approveRole: approveRole, orders: orders,
		instruments: instruments,
		logger:      logger,
		// EmitDefaultValues so a zero field (e.g. empty quality_flags) renders
		// as an explicit JSON value rather than being omitted — stable shape
		// for clients. UseProtoNames keeps snake_case matching the proto.
		marshaler: protojson.MarshalOptions{EmitDefaultValues: true, UseProtoNames: true},
	}
}

// Routes registers the v1 REST endpoints on a mux. All paths are under /v1 so
// the version is explicit in the URL (complementing the X-API-Version header
// negotiation in middleware).
//
// Every route DECLARES the capability required to reach it (SEC-M2). These are all reads:
// a scenario is a POST, but it computes a what-if and moves no capital — the HTTP verb is
// not the authority on effect.
func (h *Handler) Routes(mux *authz.Mux) {
	mux.Handle(authz.Read, "GET /v1/portfolios", h.listPortfolios)
	if h.orders != nil {
		mux.Handle(authz.Read, "GET /v1/portfolios/{id}/orders", h.listOrders)
	}
	if h.instruments != nil {
		mux.Handle(authz.Read, "GET /v1/instruments", h.listInstruments)
	}
	// THE QUEUE AN APPROVER ACTS FROM (#539). Without it the approve route is
	// answerable by nobody in practice: a held order is deliberately absent from
	// the orders table, so no other read shows it, and POST
	// /v1/orders/{id}/approve refuses an empty digest with 400 — so an approver
	// needs both an order_id and a digest that no gateway surface would give
	// them. The digest rides OrderPendingApproval on NATS, which is not a place a
	// person can look.
	//
	// authz.Approve, NOT authz.Read, and the reasoning is the override queue's:
	// this names the proposer of every unsigned change awaiting a second
	// signature and is the working surface somebody acts from, not a report.
	//
	// REGISTERED ONLY WHEN AN APPROVER IS NAMED, like every other route that
	// demands this capability. Granted to nobody it would refuse every principal
	// that exists while reading as a working control (#535).
	if h.orders != nil && h.approveRole != "" {
		mux.Handle(authz.Approve, "GET /v1/orders/pending-approvals", h.listPendingApprovals)
	}
	mux.Handle(authz.Read, "GET /v1/portfolios/{id}/exposure", h.exposure)
	mux.Handle(authz.Read, "GET /v1/portfolios/{id}/measures", h.measures)
	mux.Handle(authz.Read, "POST /v1/portfolios/{id}/scenario", h.scenario)
	mux.Handle(authz.Read, "GET /v1/health", h.health)
}

// listPortfolios answers "which portfolios may I look at" — the question every
// per-id route below assumed the caller could already answer (#399).
//
// TWO GATES APPLY, AND THEY ARE DIFFERENT QUESTIONS.
//
// The TENANT gate is writeOwned, unchanged and shared with the per-id routes: a
// reply whose owner_tenant is not the caller's is refused outright. A list makes
// that stamp matter MORE than it does on a read — a caller naming a portfolio
// already knows its id, whereas this route hands over ids they could not
// otherwise have guessed.
//
// The PORTFOLIO gate is auth.PortfolioInScope, applied per row before the reply
// is written. It is deliberately NOT PortfolioEntitled: on a read path an absent
// allow-list means the token asserted no portfolio restriction, so the tenant
// boundary is the operative limit — see pkg/auth/portfolio.go, which keeps both
// semantics side by side and says why neither may adopt the other. Getting this
// backwards here would show an unrestricted reader an EMPTY list, which reads as
// "the fund has no portfolios" rather than as a permission problem.
func (h *Handler) listPortfolios(w http.ResponseWriter, r *http.Request) {
	resp, err := h.riskFor(r).ListPortfolios(r.Context(), &querypb.ListPortfoliosRequest{})
	if err != nil {
		h.writeOwned(w, r, resp, err)
		return
	}
	// FILTERED BEFORE THE TENANT GATE RUNS, so the reply that reaches writeOwned
	// is already the one the caller may see. Mutating the response in place is
	// safe: it is this call's own value, not shared state.
	if p := middleware.PrincipalFromContext(r.Context()); p != nil {
		kept := resp.GetPortfolios()[:0]
		for _, s := range resp.GetPortfolios() {
			if auth.PortfolioInScope(p.Portfolios, s.GetPortfolioId()) {
				kept = append(kept, s)
			}
		}
		resp.Portfolios = kept
	}
	h.writeOwned(w, r, resp, nil)
}

// portfolioInScope is the CLAIM gate for a route whose path names a portfolio,
// and it returns the id so a handler cannot re-read the path and check one value
// while sending another.
//
// TWO GATES GUARD EVERY PORTFOLIO-SCOPED ROUTE, and they answer different
// questions. writeOwned reads owner_tenant off the reply and refuses another
// TENANT's data (#222). This refuses a portfolio the caller's own token does not
// name — the boundary INSIDE a tenant.
//
// # Why the risk routes now carry it too (#99)
//
// listOrders had this gate and exposure, measures and scenario did not, and the
// asymmetry was recorded here as an open question: "whether that is a gap is a
// question about those routes". It is a gap, and the deciding argument is that an
// EMPTY claim already permits.
//
// A token that carries no portfolios asserts no restriction and reads everything
// in its tenant, so nothing changes for a risk officer or for any token issued
// before the identity provider set the field. A token that DOES carry portfolios
// is an operator saying, on the invite, which portfolios this person may see —
// and the platform already honours that on GET /v1/portfolios, which hides the
// others from them, and on their order history. Serving that same person another
// portfolio's VaR, its exposure by instrument bucket, and a scenario probe
// against it does not make the restriction weaker; it makes it MEANINGLESS,
// because the only surfaces still honouring it are the two that disclose least.
//
// The counter-argument — that risk numbers are tenant-wide by nature — is
// answered by the empty case rather than by an exception here: a reader who
// should see every portfolio is given a token that says so.
//
// THE SAME 404 AS "no such portfolio" AND AS "not yours", from the same constant.
// A distinct status would tell a caller that a portfolio they may not read
// nevertheless exists, which is the oracle writeOwned is careful not to be.
func (h *Handler) portfolioInScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	p := middleware.PrincipalFromContext(r.Context())
	if p == nil || !auth.PortfolioInScope(p.Portfolios, id) {
		writeError(w, http.StatusNotFound, notFoundMsg)
		return "", false
	}
	return id, true
}

// listOrders answers "what has this portfolio traded" (#399).
//
// TWO GATES, AS ON EVERY PORTFOLIO-SCOPED ROUTE. The tenant gate is writeOwned,
// shared and unchanged — the OMS stamps owner_tenant on the reply exactly as the
// risk engine does, so a history from another tenant's OMS is refused with the
// same 404 that a missing portfolio gets, and the route is not an oracle.
//
// THE CLAIM GATE IS APPLIED HERE AND IS NOT ON THE RISK ROUTES BESIDE IT. That
// asymmetry is deliberate and worth naming rather than quietly inheriting:
// exposure, measures and scenario consult only the tenant, so a caller scoped to
// one portfolio can read another's risk in the same tenant. Whether that is a
// gap is a question about those routes (#225's neighbourhood), and widening this
// one to match would be answering it by making the new surface weaker. A
// trading history is also the most identifying of the three — it names
// instruments, sizes and times — so it is gated on the claim the token actually
// carries. Aligning the others is a separate change with its own argument.
func (h *Handler) listOrders(w http.ResponseWriter, r *http.Request) {
	id, ok := h.portfolioInScope(w, r)
	if !ok {
		return
	}
	resp, err := h.orders.ListOrders(r.Context(), &orderpb.ListOrdersRequest{
		PortfolioId: id,
		Limit:       parseLimit(r),
	})
	h.writeOwned(w, r, resp, err)
}

// listPendingApprovals answers "what is waiting on my signature" (#539).
//
// NO PORTFOLIO GATE ON THE PATH, and the filter is optional, because the OMS
// says so: ListPendingApprovalsRequest documents that an empty portfolio_id
// means "everything still awaiting a signature" — deliberately unlike
// ListOrders, because the whole-queue question is the one an approver actually
// asks. Narrowing it here would answer a question the approver did not ask and
// hide orders they are the checker for.
//
// THE TENANT GATE STILL APPLIES, through the same writeOwned as every read
// beside it. An OMS that was never given a tenant stamps an empty one, which
// fails CLOSED.
//
// AN EMPTY QUEUE IS AN ANSWER: nothing is held. A caller that rendered empty as
// "still loading" would hide a working control behind a spinner — and one that
// rendered it as "nothing to do" when the OMS was unreachable would hide a
// broken one, which is why an error here is an error and not an empty list.
func (h *Handler) listPendingApprovals(w http.ResponseWriter, r *http.Request) {
	resp, err := h.orders.ListPendingApprovals(r.Context(), &orderpb.ListPendingApprovalsRequest{
		PortfolioId: r.URL.Query().Get("portfolio"),
		Limit:       parseLimit(r),
	})
	h.writeOwned(w, r, resp, err)
}

// listInstruments answers "what can this deployment actually trade" (#406).
//
// NO PORTFOLIO GATE, AND THAT IS NOT AN OVERSIGHT. Unlike every route above it,
// this one is not about a portfolio: it reports the deployment's own
// configuration — which pairs its venue adapters hold symbol maps for. There is
// no per-portfolio answer to scope it to, and inventing one would mean deciding
// that a caller scoped to fund A may not learn that this platform trades BTC-USD,
// which is not a fact about fund A.
//
// THE TENANT GATE STILL APPLIES, through the same writeOwned as everything else.
// The catalogue describes what this tenant's OMS can route, and a reply whose
// owner_tenant is not the caller's is refused with the same 404. An OMS that was
// never given a tenant stamps an empty one, which fails CLOSED.
//
// AN EMPTY LIST IS AN ANSWER, NOT AN ERROR: this deployment can trade nothing.
// A caller that renders empty as "still loading" would hide a misconfigured
// estate behind a spinner.
func (h *Handler) listInstruments(w http.ResponseWriter, r *http.Request) {
	resp, err := h.instruments.ListTradeableInstruments(r.Context(), &venuepb.ListTradeableInstrumentsRequest{})
	h.writeOwned(w, r, resp, err)
}

// parseLimit reads ?limit=N. Anything unparseable is 0, which the OMS reads as
// "your page size" — a bad limit must not be a 400 on a read that would
// otherwise have worked.
func parseLimit(r *http.Request) int32 {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}

func (h *Handler) exposure(w http.ResponseWriter, r *http.Request) {
	id, ok := h.portfolioInScope(w, r)
	if !ok {
		return
	}
	asOf, err := parseAsOf(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid as_of: must be RFC3339")
		return
	}
	resp, err := h.riskFor(r).Exposure(r.Context(), &querypb.ExposureRequest{
		PortfolioId: id,
		AsOf:        asOf,
	})
	h.writeOwned(w, r, resp, err)
}

func (h *Handler) measures(w http.ResponseWriter, r *http.Request) {
	id, ok := h.portfolioInScope(w, r)
	if !ok {
		return
	}
	asOf, err := parseAsOf(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid as_of: must be RFC3339")
		return
	}
	resp, err := h.riskFor(r).Measures(r.Context(), &querypb.MeasuresRequest{
		PortfolioId: id,
		AsOf:        asOf,
		Measures:    r.URL.Query()["measure"], // repeatable ?measure=VaR99&measure=Delta
	})
	h.writeOwned(w, r, resp, err)
}

func (h *Handler) scenario(w http.ResponseWriter, r *http.Request) {
	var req querypb.EvaluateScenarioRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid scenario body: "+err.Error())
		return
	}
	id, ok := h.portfolioInScope(w, r)
	if !ok {
		return
	}
	req.PortfolioId = id // path wins over body, and the path is what was checked
	resp, err := h.riskFor(r).EvaluateScenario(r.Context(), &req)
	h.writeOwned(w, r, resp, err)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	resp, err := h.riskFor(r).Health(r.Context(), &querypb.HealthRequest{})
	h.write(w, r, resp, err)
}

// parseAsOf reads the optional ?as_of=<RFC3339> query param into a proto
// timestamp; absent ⇒ nil (the engine reads it as "latest").
func parseAsOf(r *http.Request) (*timestamppb.Timestamp, error) {
	v := r.URL.Query().Get("as_of")
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, err
	}
	return timestamppb.New(t), nil
}

// ownedResponse is any upstream reply that names the tenant owning the
// portfolio it describes. All three portfolio-scoped replies satisfy it —
// query.v1 declares owner_tenant on ExposureResponse, MeasuresResponse and
// EvaluateScenarioResponse — so writeOwned takes the interface rather than a
// concrete type: a NEW portfolio-scoped RPC either carries the field and is
// gated, or does not compile against this path.
type ownedResponse interface {
	proto.Message
	GetOwnerTenant() string
}

// writeOwned is write() plus the cross-tenant check, and it is the only way a
// portfolio-scoped reply reaches a client.
//
// AUTHORIZATION ON THESE ROUTES WAS ROLE-ONLY. authz.Mux checks that the caller
// holds the capability; it has no notion of which RESOURCE the capability
// applies to. Every authenticated caller holds Read (API_GATEWAY_REQUIRED_ROLE
// is kanz-user), so any of them could read any portfolio's exposure, measures
// or scenario by naming its id in the path — the id came straight from
// r.PathValue and the principal was never consulted (#222). The risk engine has
// stamped the answer on the reply since WIRE-02a and nothing read it.
//
// EMPTY OWNER DENIES. query.v1's own contract: "Empty ⇒ the engine has no
// ownership record for the portfolio, which a deny-by-default gate must treat
// as a denial, not an allow." A missing signal is not permission.
//
// That clause is DELIBERATELY REDUNDANT and cannot be mutation-killed on its
// own: with p.Tenant already proven non-empty, an empty owner_tenant fails the
// inequality anyway. It is kept because it states the contract at the point the
// contract is applied, and because it is what still denies if the p.Tenant guard
// is ever weakened. Saying so here so the next reader does not mistake belt for
// braces, or delete it believing a test covers it.
//
// 404, NOT 403, on a mismatch — and the SAME 404 the engine returns for a
// portfolio that does not exist. 403 would make this route an oracle: iterate
// ids, and the status code alone enumerates which portfolios exist in other
// tenants. Distinguishing "not yours" from "not there" leaks precisely the
// thing isolation exists to hide, which is why copilot's equivalent gate is
// careful to answer the same "regardless of tenant (no oracle)".
func (h *Handler) writeOwned(w http.ResponseWriter, r *http.Request, resp ownedResponse, err error) {
	if err != nil {
		code, msg, detail := httpStatus(err)
		h.logUpstream(r, code, detail)
		// A 404 from the engine is normalised to the SAME body the gate emits.
		// Status parity alone is not enough: if the engine's own wording came
		// through here, comparing response BODIES would still separate "exists,
		// not yours" from "does not exist" and the oracle survives in the one
		// place it is easiest to miss. Only this path is normalised — routes that
		// are not portfolio-scoped keep upstream detail.
		if code == http.StatusNotFound {
			msg = notFoundMsg
		}
		writeError(w, code, msg)
		return
	}
	p := middleware.PrincipalFromContext(r.Context())
	if p == nil || p.Tenant == "" || resp.GetOwnerTenant() == "" || resp.GetOwnerTenant() != p.Tenant {
		writeError(w, http.StatusNotFound, notFoundMsg)
		return
	}
	h.write(w, r, resp, nil)
}

// notFoundMsg is the single body a portfolio-scoped route returns for BOTH
// "no such portfolio" and "not yours". One constant, so the two cannot drift
// apart into an oracle by a later edit to either branch.
const notFoundMsg = "portfolio not found"

// write maps an upstream gRPC error to an HTTP status, or marshals the proto
// response as JSON. The gRPC code→HTTP mapping mirrors the standard
// grpc-gateway table so REST clients see conventional statuses.
//
// Not for portfolio-scoped replies — those go through writeOwned, which gates
// on ownership first. test/arch enforces that split.
func (h *Handler) write(w http.ResponseWriter, r *http.Request, resp proto.Message, err error) {
	if err != nil {
		code, msg, detail := httpStatus(err)
		h.logUpstream(r, code, detail)
		writeError(w, code, msg)
		return
	}
	body, mErr := h.marshaler.Marshal(resp)
	if mErr != nil {
		writeError(w, http.StatusInternalServerError, "response encode failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// logUpstream records the detail httpStatus withheld from the client.
//
// THIS IS NOT OPTIONAL BOOKKEEPING. Before #440 the upstream text went to the
// caller and nowhere else; now it goes to the operator and nowhere else. Drop
// this call and a 500 becomes genuinely undiagnosable — the failure would be
// silent rather than merely misdirected, which is the trade CLAUDE.md forbids
// ("nothing configured" and "checked, and fine" must never look the same).
//
// detail is empty exactly when the client already received the reason, so an
// empty detail is not a fault to log — it is a 4xx that explained itself.
//
// The path is recorded, never the query string: as_of is harmless but a query is
// caller-supplied and this line ends up in a log aggregator.
func (h *Handler) logUpstream(r *http.Request, code int, detail string) {
	if detail == "" {
		return
	}
	var subject, tenant string
	if p := middleware.PrincipalFromContext(r.Context()); p != nil {
		subject, tenant = p.Subject, p.Tenant
	}
	h.logger.Error("api-gateway: upstream call failed",
		"status", code,
		"method", r.Method,
		"path", r.URL.Path,
		"subject", subject,
		"tenant", tenant,
		"detail", detail,
	)
}

// Client-facing constants for every status the caller did not cause. They say
// what the caller can DO — retry, wait, stop — and nothing about why.
const (
	msgInternal            = "internal error"
	msgUpstreamUnavailable = "upstream unavailable"
	msgUpstreamTimeout     = "upstream timeout"
	msgNotImplemented      = "not implemented"
	msgClientClosed        = "client closed request"
)

// statusClientClosedRequest is nginx's 499, which grpc-gateway also emits for
// codes.Canceled. Not an IANA code, and used here for the same reason they use
// it: a request the CLIENT abandoned is not a fault of the server, and counting
// it as one puts the caller's own disconnects into this platform's error budget.
const statusClientClosedRequest = 499

// httpStatus maps a gRPC status onto an HTTP status, the message the CLIENT may
// see, and the detail only an OPERATOR may see.
//
// THE THIRD RETURN IS THE FIX (#440). Every arm used to return st.Message(), and
// risk-engine's mapError puts the raw error into codes.Internal:
//
//	default:
//		return status.Error(codes.Internal, err.Error())
//
// so an internal failure's text travelled upstream → here → the API client. A
// pgx error carries SQL and constraint names; a transport error carries internal
// host:port. On a multi-tenant platform neither is an API client's to read.
//
// THE RULE IS BY DIRECTION, and it is the one webhook-ingest's writePipelineError
// already follows: a 4xx describes what the CALLER sent, so it keeps its reason —
// a 400 that will not say what was wrong forces every caller to guess. A 5xx
// describes a fault of OURS, so the client gets a constant and the detail goes to
// the log. Detail is non-empty exactly when it must not be relayed.
//
// THE TABLE IS NOW THE WHOLE grpc-gateway TABLE, which is what the comment on
// write has always claimed it mirrored. It mirrored six of sixteen codes; the
// rest fell to a default answering 500. That is the same defect class as #434 and
// #439 one service over — a 5xx is RETRYABLE and blames this platform, so a
// client cancellation and a caller-side precondition failure both read as "kanz
// is broken" and both invite a retry that cannot help.
func httpStatus(err error) (code int, clientMsg, detail string) {
	st, ok := status.FromError(err)
	if !ok {
		// Not a gRPC status at all — a dial or transport failure, whose text IS the
		// internal topology ("dial tcp 10.0.3.12:9090: connect: connection
		// refused"). This was the worst of the three leaks and has no 4xx reading.
		return http.StatusInternalServerError, msgInternal, err.Error()
	}
	switch st.Code() {
	// --- The caller caused it: they may read why. -------------------------------
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return http.StatusBadRequest, st.Message(), ""
	case codes.Unauthenticated:
		return http.StatusUnauthorized, st.Message(), ""
	case codes.PermissionDenied:
		return http.StatusForbidden, st.Message(), ""
	case codes.NotFound:
		// writeOwned normalises this one further on portfolio-scoped routes, so the
		// body cannot separate "not yours" from "not there". See notFoundMsg.
		return http.StatusNotFound, st.Message(), ""
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict, st.Message(), ""
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests, st.Message(), ""
	case codes.Canceled:
		// Nobody is listening — the client hung up. A constant, because the message
		// is never read and a leak that is never read is still a leak in a log-
		// replaying proxy.
		return statusClientClosedRequest, msgClientClosed, ""

	// --- We caused it: constant out, detail to the operator. ---------------------
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout, msgUpstreamTimeout, st.Message()
	case codes.Unavailable:
		return http.StatusServiceUnavailable, msgUpstreamUnavailable, st.Message()
	case codes.Unimplemented:
		return http.StatusNotImplemented, msgNotImplemented, st.Message()
	case codes.Internal, codes.Unknown, codes.DataLoss:
		return http.StatusInternalServerError, msgInternal, st.Message()

	default:
		// UNREACHABLE FOR EVERY CODE gRPC DEFINES TODAY — the arms above are
		// exhaustive, and test/arch/gateway_grpc_code_table_test.go fails the build
		// if any kanz server starts returning one that is missing. It is LOUD rather
		// than silent so that if gRPC adds a code, the log names it instead of it
		// becoming an anonymous 500.
		return http.StatusInternalServerError, msgInternal,
			fmt.Sprintf("unmapped gRPC code %s: %s", st.Code(), st.Message())
	}
}

// errBodyTooLarge guards the scenario JSON decode.
var errBodyTooLarge = errors.New("request body too large")

// riskFor returns the risk-engine client that owns this request's tenant.
//
// The tenant comes from the PRINCIPAL this gateway authenticated and injected,
// never from anything the client sent, so a caller cannot choose whose book it
// is answered from. No principal — an unauthenticated path, which the risk
// routes do not have — falls back to the platform engine, the same answer a
// tenant with no rendered engine gets.
func (h *Handler) riskFor(r *http.Request) querypb.RiskQueryServiceClient {
	tenant := ""
	if p := middleware.PrincipalFromContext(r.Context()); p != nil {
		tenant = p.Tenant
	}
	return h.risk.clientFor(tenant)
}

// WithPerTenantRisk attaches the per-tenant risk-engine clients, for tenants
// that have their own rendered engine.
//
// BUILT ONCE AT STARTUP by the composition root and never mutated afterwards —
// see riskclients.go for why a lazily-grown map keyed on a request-supplied
// tenant would be a leak with an external trigger.
func (h *Handler) WithPerTenantRisk(perTenant map[string]querypb.RiskQueryServiceClient) (*Handler, error) {
	risk, err := h.risk.withPerTenant(perTenant)
	if err != nil {
		return nil, err
	}
	h.risk = risk
	return h, nil
}
