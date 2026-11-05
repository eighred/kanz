package order

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
)

func TestFillFoldCaptureDetachedAndBounded(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 123, time.UTC)
	state := &orderpb.OrderState{OrderId: "order", Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED, OrderedQuantity: &commonpb.Decimal{Coefficient: 10}, LeavesQuantity: &commonpb.Decimal{Coefficient: 10}}
	fill := &orderpb.Fill{Quantity: &commonpb.Decimal{Coefficient: 1}, Price: &commonpb.Decimal{Coefficient: 2}}
	candidate, e := ApplyFill(state, fill, at)
	if e != nil {
		t.Fatal(e)
	}
	var capture FillFoldCapture
	ctx := bus.WithTenantID(context.Background(), "tenant")
	observeFillFold(ctx, func(c FillFoldCapture) { capture = c }, "recovery", 42, state, fill, at, candidate, nil)
	if capture.Tenant != "tenant" || capture.ExpectedVersion != 42 || !capture.EventTime.Equal(at) || capture.CapturedAt.IsZero() {
		t.Fatal("attribution lost")
	}
	state.OrderId = "changed"
	fill.Quantity.Coefficient = 5
	candidate.OrderId = "changed"
	if capture.State.OrderId != "order" || capture.Fill.Quantity.Coefficient != 1 || capture.Candidate.OrderId != "order" {
		t.Fatal("capture aliases live fold")
	}
	state.ProtoReflect().SetUnknown(make([]byte, 1<<20))
	observeFillFold(ctx, func(c FillFoldCapture) { capture = c }, "live", 42, state, fill, at, candidate, nil)
	if !capture.Unsupported || capture.State != nil || capture.Fill != nil || capture.Candidate != nil {
		t.Fatal("oversized capture copied")
	}
}
func TestFillFoldCaptureRefusal(t *testing.T) {
	var c FillFoldCapture
	observeFillFold(context.Background(), func(v FillFoldCapture) { c = v }, "live", 1, &orderpb.OrderState{}, &orderpb.Fill{}, time.Now(), nil, reject("OVERFILL", "fixture"))
	if c.RejectionCode != "OVERFILL" || c.Candidate != nil || c.NumericalFailure {
		t.Fatal("aggregate refusal changed")
	}
}
