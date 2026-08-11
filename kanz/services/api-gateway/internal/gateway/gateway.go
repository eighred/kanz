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
	// instruments is the OMS's tradeable-pair catalogue, or nil for the same
	// reason (#406). It rides the SAME OMS connection as orders — one upstream,
	// two contracts — so a deployment fronting an OMS has both or neither.
	instruments venuepb.VenueQueryServiceClient
	client      querypb.RiskQueryServiceClient
	marshaler   protojson.MarshalOptions
}

// New returns a Handler over the risk-engine client and, optionally, the OMS's
// order-history client.
//
// orders MAY BE NIL, and then the history route is not registered at all (#399).
// A deployment whose OMS serves no read surface should 404 that path rather than
// answer it with an error: an unregistered route says "not configured here",
// while a registered one that always fails says "broken", and only one of those
// is true. It is the same shape the control routes take for an absent operator.
func New(client querypb.RiskQueryServiceClient, orders orderpb.OrderQueryServiceClient, instruments venuepb.VenueQueryServiceClient) *Handler {
	return &Handler{
		client:      client,
		orders:      orders,
		instruments: instruments,
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
	resp, err := h.client.ListPortfolios(r.Context(), &querypb.ListPortfoliosRequest{})
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
	id := r.PathValue("id")
	p := middleware.PrincipalFromContext(r.Context())
	if p == nil || !auth.PortfolioInScope(p.Portfolios, id) {
		// The SAME 404 as "no such portfolio" and as "not yours", from the same
		// constant. A distinct status here would tell a caller that a portfolio
		// they may not read nevertheless exists.
		writeError(w, http.StatusNotFound, notFoundMsg)
		return
	}
	resp, err := h.orders.ListOrders(r.Context(), &orderpb.ListOrdersRequest{
		PortfolioId: id,
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
	asOf, err := parseAsOf(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid as_of: must be RFC3339")
		return
	}
	resp, err := h.client.Exposure(r.Context(), &querypb.ExposureRequest{
		PortfolioId: r.PathValue("id"),
		AsOf:        asOf,
	})
	h.writeOwned(w, r, resp, err)
}

func (h *Handler) measures(w http.ResponseWriter, r *http.Request) {
	asOf, err := parseAsOf(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid as_of: must be RFC3339")
		return
	}
	resp, err := h.client.Measures(r.Context(), &querypb.MeasuresRequest{
		PortfolioId: r.PathValue("id"),
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
	req.PortfolioId = r.PathValue("id") // path wins over body
	resp, err := h.client.EvaluateScenario(r.Context(), &req)
	h.writeOwned(w, r, resp, err)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.Health(r.Context(), &querypb.HealthRequest{})
	h.write(w, resp, err)
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
		code, msg := httpStatus(err)
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
	h.write(w, resp, nil)
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
func (h *Handler) write(w http.ResponseWriter, resp proto.Message, err error) {
	if err != nil {
		code, msg := httpStatus(err)
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

// httpStatus maps a gRPC status to an HTTP status code + client message.
func httpStatus(err error) (int, string) {
	st, ok := status.FromError(err)
	if !ok {
		return http.StatusInternalServerError, err.Error()
	}
	switch st.Code() {
	case codes.NotFound:
		return http.StatusNotFound, st.Message()
	case codes.InvalidArgument:
		return http.StatusBadRequest, st.Message()
	case codes.PermissionDenied:
		return http.StatusForbidden, st.Message()
	case codes.Unauthenticated:
		return http.StatusUnauthorized, st.Message()
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout, st.Message()
	case codes.Unavailable:
		return http.StatusServiceUnavailable, st.Message()
	default:
		return http.StatusInternalServerError, st.Message()
	}
}

// errBodyTooLarge guards the scenario JSON decode.
var errBodyTooLarge = errors.New("request body too large")
