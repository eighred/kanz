package foldshadow

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/eighred/kanz/internal/shadowprocess"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	omspb "github.com/eighred/kanz/kanz-schemas-go/oms/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (w *Worker) SubmitTerminal(c order.TerminalFoldCapture) {
	if w == nil {
		return
	}
	select {
	case <-w.done:
		return
	default:
	}
	if c.Unsupported || c.State == nil || proto.Size(c.State)+proto.Size(c.Candidate)+proto.Size(c.UnfilledQuantity) > shadowprocess.MaxBytes-1024 {
		w.observe("invalid_input")
		return
	}
	c.State = proto.Clone(c.State).(*orderpb.OrderState)
	if c.Candidate != nil {
		c.Candidate = proto.Clone(c.Candidate).(*orderpb.OrderState)
	}
	if c.UnfilledQuantity != nil {
		c.UnfilledQuantity = proto.Clone(c.UnfilledQuantity).(*commonpb.Decimal)
	}
	select {
	case w.queue <- capturedWork{terminal: &c}:
	default:
		w.observe("queue_full")
	}
}

func terminalJob(c order.TerminalFoldCapture, tenant, revision string) (*omspb.TerminalFoldRequest, error) {
	bad := errors.New("invalid captured terminal fold")
	if c.Unsupported || c.State == nil || c.Tenant != tenant || !identifier(tenant) || !identifier(revision) || c.ExpectedVersion < 0 || c.EventTime.IsZero() || c.CapturedAt.IsZero() || unknown(c.State.ProtoReflect()) {
		return nil, bad
	}
	var op omspb.TerminalFoldOperation
	switch c.Operation {
	case "cancel":
		op = omspb.TerminalFoldOperation_TERMINAL_FOLD_OPERATION_CANCEL
	case "expire":
		op = omspb.TerminalFoldOperation_TERMINAL_FOLD_OPERATION_EXPIRE
	default:
		return nil, bad
	}
	if c.Path != "reconcile" && !(c.Path == "cancel" && c.Operation == "cancel") && !(c.Path == "schedule" && c.Operation == "expire") {
		return nil, bad
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	j := &omspb.TerminalFoldRequest{CaptureId: hex.EncodeToString(id[:]), TenantId: tenant, SourceRevision: revision, ExpectedVersion: proto.Int64(c.ExpectedVersion), State: c.State, EventTime: timestamppb.New(c.EventTime), CapturedAt: timestamppb.New(c.CapturedAt), Operation: op, Path: c.Path}
	if j.EventTime.CheckValid() != nil || j.CapturedAt.CheckValid() != nil || proto.Size(j) > shadowprocess.MaxBytes-64 {
		return nil, bad
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(j)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(wire)
	j.CaptureDigest = digest[:]
	return j, nil
}
func compareTerminal(j *omspb.TerminalFoldRequest, c order.TerminalFoldCapture, r *omspb.TerminalFoldResult, version string) string {
	if unknown(r.ProtoReflect()) || r.CaptureId != j.CaptureId || !bytes.Equal(r.CaptureDigest, j.CaptureDigest) || r.TenantId != j.TenantId || r.ProducerVersion != version || r.ExpectedVersion == nil || r.GetExpectedVersion() != j.GetExpectedVersion() || r.Operation != j.Operation {
		return "invalid_result"
	}
	switch r.Status {
	case omspb.TerminalFoldStatus_TERMINAL_FOLD_STATUS_OK:
		if r.Candidate == nil || r.RejectionCode != "" {
			return "invalid_result"
		}
		if c.RejectionCode != "" || !proto.Equal(c.Candidate, r.Candidate) || !proto.Equal(c.UnfilledQuantity, r.UnfilledQuantity) {
			return "disagreement"
		}
		return "matched"
	case omspb.TerminalFoldStatus_TERMINAL_FOLD_STATUS_REJECTED:
		if r.Candidate != nil || r.UnfilledQuantity != nil || r.RejectionCode != "ORDER_TERMINAL" {
			return "invalid_result"
		}
		if c.Candidate != nil || c.UnfilledQuantity != nil || c.RejectionCode != r.RejectionCode {
			return "disagreement"
		}
		return "matched"
	case omspb.TerminalFoldStatus_TERMINAL_FOLD_STATUS_UNSUPPORTED_INPUT:
		if r.Candidate != nil || r.UnfilledQuantity != nil || r.RejectionCode != "" {
			return "invalid_result"
		}
		return "worker_rejected"
	default:
		return "invalid_result"
	}
}
func evaluateTerminal(ctx context.Context, c Config, tenant, revision string, capture order.TerminalFoldCapture) string {
	j, err := terminalJob(capture, tenant, revision)
	if err != nil {
		return "invalid_input"
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(j)
	if err != nil {
		return "invalid_input"
	}
	raw, status := shadowprocess.Run(ctx, shadowprocess.Config{Executable: c.Executable, Args: []string{"--terminal"}, Timeout: c.Timeout}, wire)
	if status != "" {
		return status
	}
	r := new(omspb.TerminalFoldResult)
	if proto.Unmarshal(raw, r) != nil {
		return "invalid_result"
	}
	return compareTerminal(j, capture, r, c.ExpectedVersion)
}
