// Package control fronts the operator's control plane at the gateway (OPS-M2b).
//
// WHY THIS EXISTS. The operator.v1 service provisions, cordons, drains and relabels
// nodes, and writes the exchange API credentials the venue adapters sign with. It
// used to be reachable only by `kubectl port-forward` against a plaintext listener,
// which meant two things at once: the RPC authenticated nobody, and every operator
// needed a cluster credential to do routine work. OPS-M2a fixed the first half by
// making the operator demand an SVID from a named caller. This is the second half:
// that named caller is the gateway, so a human authenticates as a PERSON — OIDC in
// production — and never holds a kubeconfig.
//
// THE HTTP CONTRACT IS THE PROTO CONTRACT. Requests and responses are marshalled
// with protojson rather than through hand-written DTOs. That is not laziness about
// API design: a second set of structs would be a second definition of the same
// messages, free to drift from the proto the operator actually serves, and the drift
// would surface as a field silently dropped between the TUI and a node provision.
// One definition, generated, with the gateway as transport.
//
// EVERY ROUTE HERE IS authz.Operate. None is Read and none is Trade — see the
// capability's own comment. A future route added to this file must decide the same
// question, because authz.Mux.Handle will not compile without a capability.
package control

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	operatorpb "github.com/kanz-eng/kanz-schemas-go/operator/v1"

	"github.com/kanz-eng/kanz/services/api-gateway/internal/authz"
)

// callTimeout bounds every control-plane RPC. AddNode returns as soon as the Job is
// created (the SSH work is asynchronous), so no route here is long-running — a
// request that outlives this is a control plane that is not answering, and the
// caller should be told so rather than left hanging.
const callTimeout = 30 * time.Second

// maxBody caps a control request. These bodies are hostnames, node names and API
// keys; a megabyte is already absurd for all of them.
const maxBody = 1 << 20

// Handler serves the /v1/control routes by forwarding to operator.v1.
type Handler struct {
	client operatorpb.OperatorServiceClient
	logger *slog.Logger
}

// New returns a Handler forwarding to client.
func New(client operatorpb.OperatorServiceClient, logger *slog.Logger) *Handler {
	return &Handler{client: client, logger: logger}
}

// Routes registers the control plane. Read and Trade are deliberately absent.
func (h *Handler) Routes(m *authz.Mux) {
	m.Handle(authz.Operate, "GET /v1/control/nodes", h.listNodes)
	m.Handle(authz.Operate, "GET /v1/control/clusters", h.listClusters)
	m.Handle(authz.Operate, "POST /v1/control/nodes", h.addNode)
	m.Handle(authz.Operate, "GET /v1/control/provisions", h.listProvisions)
	m.Handle(authz.Operate, "POST /v1/control/test-connection", h.testConnection)
	m.Handle(authz.Operate, "POST /v1/control/nodes/{name}/cordon", h.cordon)
	m.Handle(authz.Operate, "POST /v1/control/nodes/{name}/uncordon", h.uncordon)
	m.Handle(authz.Operate, "POST /v1/control/nodes/{name}/drain", h.drain)
	m.Handle(authz.Operate, "POST /v1/control/nodes/{name}/region", h.setRegion)
	m.Handle(authz.Operate, "GET /v1/control/venues", h.listVenueKeys)
	m.Handle(authz.Operate, "PUT /v1/control/venues/{venue}/keys", h.setVenueKeys)
}

func (h *Handler) listNodes(w http.ResponseWriter, r *http.Request) {
	h.forward(w, r, func(ctx context.Context) (proto.Message, error) {
		return h.client.ListNodes(ctx, &operatorpb.ListNodesRequest{})
	})
}

func (h *Handler) listClusters(w http.ResponseWriter, r *http.Request) {
	h.forward(w, r, func(ctx context.Context) (proto.Message, error) {
		return h.client.ListClusters(ctx, &operatorpb.ListClustersRequest{})
	})
}

func (h *Handler) addNode(w http.ResponseWriter, r *http.Request) {
	var req operatorpb.AddNodeRequest
	if !h.decode(w, r, &req) {
		return
	}
	h.forward(w, r, func(ctx context.Context) (proto.Message, error) {
		return h.client.AddNode(ctx, &req)
	})
}

func (h *Handler) listProvisions(w http.ResponseWriter, r *http.Request) {
	h.forward(w, r, func(ctx context.Context) (proto.Message, error) {
		return h.client.ListProvisions(ctx, &operatorpb.ListProvisionsRequest{})
	})
}

func (h *Handler) testConnection(w http.ResponseWriter, r *http.Request) {
	var req operatorpb.TestConnectionRequest
	if !h.decode(w, r, &req) {
		return
	}
	h.forward(w, r, func(ctx context.Context) (proto.Message, error) {
		return h.client.TestConnection(ctx, &req)
	})
}

func (h *Handler) cordon(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	h.forward(w, r, func(ctx context.Context) (proto.Message, error) {
		return h.client.Cordon(ctx, &operatorpb.CordonRequest{Name: name})
	})
}

func (h *Handler) uncordon(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	h.forward(w, r, func(ctx context.Context) (proto.Message, error) {
		return h.client.Uncordon(ctx, &operatorpb.UncordonRequest{Name: name})
	})
}

func (h *Handler) drain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	h.forward(w, r, func(ctx context.Context) (proto.Message, error) {
		return h.client.Drain(ctx, &operatorpb.DrainRequest{Name: name})
	})
}

func (h *Handler) setRegion(w http.ResponseWriter, r *http.Request) {
	var req operatorpb.SetNodeRegionRequest
	if !h.decode(w, r, &req) {
		return
	}
	// The node is addressed by the PATH, never by the body — two spellings of the
	// same identity is a way to drain the node you were not looking at.
	req.Name = r.PathValue("name")
	h.forward(w, r, func(ctx context.Context) (proto.Message, error) {
		return h.client.SetNodeRegion(ctx, &req)
	})
}

func (h *Handler) listVenueKeys(w http.ResponseWriter, r *http.Request) {
	h.forward(w, r, func(ctx context.Context) (proto.Message, error) {
		return h.client.ListVenueKeys(ctx, &operatorpb.ListVenueKeysRequest{})
	})
}

// setVenueKeys carries the most dangerous body on this surface: an exchange API key
// and secret. Nothing in this path logs the request — not on success, not on error.
// The operator's own response carries the exchange-proved account id (S4b) and never
// echoes the credential back.
func (h *Handler) setVenueKeys(w http.ResponseWriter, r *http.Request) {
	var req operatorpb.SetVenueKeysRequest
	if !h.decode(w, r, &req) {
		return
	}
	req.Venue = r.PathValue("venue")
	h.forward(w, r, func(ctx context.Context) (proto.Message, error) {
		return h.client.SetVenueKeys(ctx, &req)
	})
}

// decode reads a protojson body into msg. It reports whether the caller should
// continue; on failure it has already written the response.
func (h *Handler) decode(w http.ResponseWriter, r *http.Request, msg proto.Message) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unreadable request body")
		return false
	}
	if len(body) == 0 {
		return true // an empty body is a valid zero-valued request
	}
	// DiscardUnknown is deliberately OFF: a caller sending a field this gateway does
	// not know is running against a different contract, and silently dropping it
	// would provision something other than what was asked for.
	if err := protojson.Unmarshal(body, msg); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request body")
		return false
	}
	return true
}

// forward runs fn under a bounded context and renders the result.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, fn func(context.Context) (proto.Message, error)) {
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()

	resp, err := fn(ctx)
	if err != nil {
		code, msg := translate(err)
		// Logged WITHOUT the request body — see setVenueKeys.
		h.logger.Warn("control-plane call failed", "route", r.Pattern, "status", code, "err", err)
		writeErr(w, code, msg)
		return
	}
	out, err := protojson.Marshal(resp)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// translate maps a gRPC status onto HTTP without leaking the operator's internals.
//
// Unimplemented is 501 and NOT 500 on purpose: it is what the operator returns when
// a capability is deliberately unconfigured (no provisioner image ⇒ AddNode off, no
// secret backend ⇒ SetVenueKeys off). That is a deployment decision, and reporting
// it as a server fault would send someone hunting a bug that is a missing env var.
func translate(err error) (int, string) {
	st, ok := status.FromError(err)
	if !ok {
		return http.StatusBadGateway, "control plane unavailable"
	}
	switch st.Code() {
	case codes.InvalidArgument:
		return http.StatusBadRequest, st.Message()
	case codes.NotFound:
		return http.StatusNotFound, st.Message()
	case codes.AlreadyExists:
		return http.StatusConflict, st.Message()
	case codes.FailedPrecondition:
		// S4b: the exchange rejected the credentials. The operator has already
		// sanitized this message; it is the one thing the caller most needs.
		return http.StatusPreconditionFailed, st.Message()
	case codes.PermissionDenied:
		return http.StatusForbidden, "insufficient capability"
	case codes.Unimplemented:
		return http.StatusNotImplemented, st.Message()
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout, "control plane did not answer in time"
	case codes.Unavailable:
		return http.StatusBadGateway, "control plane unavailable"
	default:
		return http.StatusInternalServerError, "control plane error"
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
