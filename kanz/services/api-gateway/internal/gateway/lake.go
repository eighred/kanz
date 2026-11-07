package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/services/api-gateway/internal/authz"
	"github.com/eighred/kanz/services/api-gateway/internal/middleware"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// WithLakeSnapshots binds one configured tenant's optional finite snapshot owner.
// No caller controls the upstream or delegated Caller message.
func (h *Handler) WithLakeSnapshots(client lakepb.LakeSnapshotServiceClient, tenant string) error {
	if client == nil || tenant == "" || h.lake != nil {
		return errors.New("lake snapshot client requires one explicit tenant")
	}
	h.lake, h.lakeTenant = client, tenant
	return nil
}

func (h *Handler) lakeRoutes(mux *authz.Mux) {
	if h.lake == nil {
		return
	}
	mux.Handle(authz.LakeSnapshotCreate, "POST /v1/lake/snapshots", h.createLakeSnapshot)
	mux.Handle(authz.LakeSnapshotRead, "GET /v1/lake/snapshots/{id}", h.getLakeSnapshot)
	mux.Handle(authz.LakeSnapshotRead, "POST /v1/lake/snapshots/{id}/query", h.queryLakeSnapshot)
}

func (h *Handler) lakeCaller(r *http.Request) *lakepb.GatewayCaller {
	p := middleware.PrincipalFromContext(r.Context())
	if p == nil || p.Subject == "" || p.Tenant != h.lakeTenant || len(p.Portfolios) != 0 {
		return nil
	}
	return &lakepb.GatewayCaller{Subject: p.Subject, Tenant: p.Tenant, Roles: append([]string(nil), p.Roles...)}
}

func lakeInput(w http.ResponseWriter, r *http.Request, message proto.Message) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	raw, err := io.ReadAll(r.Body)
	if err != nil || (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, message) != nil {
		writeError(w, http.StatusBadRequest, "invalid snapshot input")
		return false
	}
	return true
}

func (h *Handler) createLakeSnapshot(w http.ResponseWriter, r *http.Request) {
	caller := h.lakeCaller(r)
	if caller == nil {
		writeError(w, http.StatusNotFound, "snapshot unavailable")
		return
	}
	// Decode ONLY business input. A JSON caller/roles/tenant injection is an
	// unknown field and refuses; internal delegation is constructed afterward.
	input := new(lakepb.CreateSnapshotInput)
	if !lakeInput(w, r, input) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	response, err := h.lake.CreateSnapshot(ctx, &lakepb.CreateSnapshotRequest{Caller: caller, Input: input})
	h.writeLake(w, response, err)
}

func (h *Handler) getLakeSnapshot(w http.ResponseWriter, r *http.Request) {
	caller := h.lakeCaller(r)
	if caller == nil {
		writeError(w, http.StatusNotFound, "snapshot unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	response, err := h.lake.GetSnapshot(ctx, &lakepb.GetSnapshotRequest{Caller: caller, SnapshotId: r.PathValue("id")})
	h.writeLake(w, response, err)
}

func (h *Handler) queryLakeSnapshot(w http.ResponseWriter, r *http.Request) {
	caller := h.lakeCaller(r)
	if caller == nil {
		writeError(w, http.StatusNotFound, "snapshot unavailable")
		return
	}
	query := new(lakepb.SnapshotQuery)
	if !lakeInput(w, r, query) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	response, err := h.lake.QuerySnapshot(ctx, &lakepb.QuerySnapshotRequest{Caller: caller, SnapshotId: r.PathValue("id"), Query: query})
	h.writeLake(w, response, err)
}

func (h *Handler) writeLake(w http.ResponseWriter, response proto.Message, err error) {
	if err != nil {
		code, message, _ := httpStatus(err)
		writeError(w, code, message)
		return
	}
	if response == nil || !response.ProtoReflect().IsValid() || proto.Size(response) > 1<<20 {
		writeError(w, http.StatusBadGateway, "snapshot response unavailable")
		return
	}
	raw, err := h.marshaler.Marshal(response)
	if err != nil || len(raw) > 1<<20 {
		writeError(w, http.StatusBadGateway, "snapshot response unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}
