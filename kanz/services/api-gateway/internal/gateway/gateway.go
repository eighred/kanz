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
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
)

// Handler serves the REST surface by forwarding to a RiskQueryServiceClient.
// It depends on the generated client INTERFACE, so tests inject a fake and the
// real gateway injects an mTLS-dialed client (main).
type Handler struct {
	client    querypb.RiskQueryServiceClient
	marshaler protojson.MarshalOptions
}

// New returns a Handler over the gRPC client.
func New(client querypb.RiskQueryServiceClient) *Handler {
	return &Handler{
		client: client,
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
	mux.Handle(authz.Read, "GET /v1/portfolios/{id}/exposure", h.exposure)
	mux.Handle(authz.Read, "GET /v1/portfolios/{id}/measures", h.measures)
	mux.Handle(authz.Read, "POST /v1/portfolios/{id}/scenario", h.scenario)
	mux.Handle(authz.Read, "GET /v1/health", h.health)
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
