package order

import (
	"context"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"testing"
	"time"
)

func TestTerminalCaptureDetachedBoundedAndOptional(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 123, time.UTC)
	ctx := bus.WithTenantID(context.Background(), "tenant")
	for _, op := range []string{"cancel", "expire"} {
		s := &orderpb.OrderState{OrderId: "order", Status: orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, LeavesQuantity: &commonpb.Decimal{Coefficient: 8}}
		var next *orderpb.OrderState
		var quantity *commonpb.Decimal
		var err error
		if op == "cancel" {
			next, quantity, err = Cancel(s, at)
		} else {
			next, quantity, err = Expire(s, at)
		}
		var capture TerminalFoldCapture
		observeTerminalFold(ctx, func(c TerminalFoldCapture) { capture = c }, op, "reconcile", 7, s, next, quantity, at, err)
		if capture.Tenant != "tenant" || capture.Operation != op || capture.ExpectedVersion != 7 || !capture.EventTime.Equal(at) || capture.CapturedAt.IsZero() {
			t.Fatal("lost attribution")
		}
		s.OrderId = "changed"
		next.OrderId = "changed"
		quantity.Coefficient = 3
		if capture.State.OrderId != "order" || capture.Candidate.OrderId != "order" || capture.UnfilledQuantity.Coefficient != 8 {
			t.Fatal("capture aliases pure state")
		}
		s.ProtoReflect().SetUnknown(make([]byte, 1<<20))
		observeTerminalFold(ctx, func(c TerminalFoldCapture) { capture = c }, op, "reconcile", 7, s, next, quantity, at, nil)
		if !capture.Unsupported || capture.State != nil || capture.Candidate != nil {
			t.Fatal("oversized state cloned")
		}
	}
	s := &orderpb.OrderState{}
	next, quantity, err := Cancel(s, at)
	var c TerminalFoldCapture
	observeTerminalFold(ctx, func(v TerminalFoldCapture) { c = v }, "cancel", "cancel", 1, s, next, quantity, at, err)
	if c.UnfilledQuantity != nil {
		t.Fatal("missing quantity became zero")
	}
	_, quantity, err = Cancel(next, at)
	observeTerminalFold(ctx, func(v TerminalFoldCapture) { c = v }, "cancel", "cancel", 2, next, nil, quantity, at, err)
	if c.RejectionCode != "ORDER_TERMINAL" || c.Candidate != nil || c.UnfilledQuantity != nil {
		t.Fatal("refusal became candidate")
	}
}
