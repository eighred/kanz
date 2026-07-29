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
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
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
	h.write(w, resp, err)
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
	h.write(w, resp, err)
}

func (h *Handler) scenario(w http.ResponseWriter, r *http.Request) {
	var req querypb.EvaluateScenarioRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid scenario body: "+err.Error())
		return
	}
	req.PortfolioId = r.PathValue("id") // path wins over body
	resp, err := h.client.EvaluateScenario(r.Context(), &req)
	h.write(w, resp, err)
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

// write maps an upstream gRPC error to an HTTP status, or marshals the proto
// response as JSON. The gRPC code→HTTP mapping mirrors the standard
// grpc-gateway table so REST clients see conventional statuses.
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
