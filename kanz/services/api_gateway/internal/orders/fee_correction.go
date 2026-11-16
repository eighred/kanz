package orders

import (
	"encoding/hex"
	"io"
	"net/http"
	"strings"

	"github.com/eighred/kanz/internal/dualcontrol"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
)

func (h *Handler) proposeFeeCorrection(w http.ResponseWriter, r *http.Request) {
	h.feeCorrection(w, r, false)
}
func (h *Handler) approveFeeCorrection(w http.ResponseWriter, r *http.Request) {
	h.feeCorrection(w, r, true)
}

func (h *Handler) feeCorrection(w http.ResponseWriter, r *http.Request, approve bool) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	// Correcting a booked fee cannot submit an order and remains available while
	// trading is halted. Capability and portfolio gates still apply.
	body, err := io.ReadAll(io.LimitReader(r.Body, 65537))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}
	if len(body) > 65536 {
		writeError(w, http.StatusRequestEntityTooLarge, "fee correction body too large")
		return
	}
	orderID, caseID := r.PathValue("id"), r.PathValue("case")
	if strings.TrimSpace(orderID) == "" || len(orderID) > 256 || strings.TrimSpace(caseID) == "" || len(caseID) > 256 {
		writeError(w, http.StatusBadRequest, "order and recovery case are required")
		return
	}
	var message proto.Message
	subject := "order.order.propose_fee_correction"
	if approve {
		var cmd orderpb.ApproveExecutionFeeCorrection
		if err := h.unmarshal.Unmarshal(body, &cmd); err != nil {
			writeError(w, http.StatusBadRequest, "invalid fee approval body")
			return
		}
		digest, err := hex.DecodeString(cmd.Digest)
		if err != nil || len(digest) != 32 {
			writeError(w, http.StatusBadRequest, "exact proposal digest is required")
			return
		}
		cmd.OrderId, cmd.CaseId, cmd.Metadata = orderID, caseID, bindMetadata(cmd.Metadata, p, orderID)
		message, subject = &cmd, "order.order.approve_fee_correction"
	} else {
		var cmd orderpb.ProposeExecutionFeeCorrection
		if err := h.unmarshal.Unmarshal(body, &cmd); err != nil {
			writeError(w, http.StatusBadRequest, "invalid fee proposal body")
			return
		}
		if strings.TrimSpace(cmd.Reason) == "" || len(cmd.Reason) > 2048 {
			writeError(w, http.StatusBadRequest, "a bounded correction reason is required")
			return
		}
		cmd.OrderId, cmd.CaseId, cmd.Metadata = orderID, caseID, bindMetadata(cmd.Metadata, p, orderID)
		message = &cmd
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid fee correction")
		return
	}
	idem := dualcontrol.Digest(subject, p.Tenant, p.Subject, orderID, caseID, string(encoded), r.Header.Get("Idempotency-Key"))
	if err := h.publish(r.Context(), p, subject, orderID, message, idem); err != nil {
		writePublishError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"order_id": orderID, "case_id": caseID, "status": "submitted"})
}
