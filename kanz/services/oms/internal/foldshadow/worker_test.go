package foldshadow

import (
	"context"
	"os"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	omspb "github.com/eighred/kanz/kanz-schemas-go/oms/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func fixture(t *testing.T) order.FillFoldCapture {
	t.Helper()
	at := time.Date(2026, 1, 1, 0, 0, 0, 123, time.UTC)
	state := &orderpb.OrderState{OrderId: "order", PortfolioId: "portfolio", Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED, OrderedQuantity: &commonpb.Decimal{Coefficient: 10}, LeavesQuantity: &commonpb.Decimal{Coefficient: 10}}
	fill := &orderpb.Fill{OrderId: "order", FillId: "fill", Quantity: &commonpb.Decimal{Coefficient: 2}, Price: &commonpb.Decimal{Coefficient: 3}, ExecutedAt: timestamppb.New(at)}
	next, e := order.ApplyFill(state, fill, at)
	if e != nil {
		t.Fatal(e)
	}
	return order.FillFoldCapture{Tenant: "tenant", Path: "live", ExpectedVersion: 1, State: state, Fill: fill, Candidate: next, EventTime: at, CapturedAt: at.Add(time.Second)}
}
func response(j *omspb.FillFoldRequest, c order.FillFoldCapture) *omspb.FillFoldResult {
	return &omspb.FillFoldResult{CaptureId: j.CaptureId, CaptureDigest: j.CaptureDigest, TenantId: j.TenantId, ProducerVersion: "stamp", ExpectedVersion: j.ExpectedVersion, Status: omspb.FillFoldStatus_FILL_FOLD_STATUS_OK, Candidate: proto.Clone(c.Candidate).(*orderpb.OrderState)}
}
func TestFoldResultBindingAndUnknowns(t *testing.T) {
	c := fixture(t)
	j, e := job(c, "tenant", "go")
	if e != nil {
		t.Fatal(e)
	}
	if !j.EventTime.AsTime().Equal(c.EventTime) || !j.CapturedAt.AsTime().Equal(c.CapturedAt) {
		t.Fatal("timestamps replaced")
	}
	if s := compare(j, c, response(j, c), "stamp"); s != "matched" {
		t.Fatal(s)
	}
	cases := map[string]func(*omspb.FillFoldResult){"tenant": func(r *omspb.FillFoldResult) { r.TenantId = "other" }, "stamp": func(r *omspb.FillFoldResult) { r.ProducerVersion = "wrong" }, "digest": func(r *omspb.FillFoldResult) { r.CaptureDigest = nil }, "version absent": func(r *omspb.FillFoldResult) { r.ExpectedVersion = nil }, "version changed": func(r *omspb.FillFoldResult) { r.ExpectedVersion = proto.Int64(2) }, "nested unknown": func(r *omspb.FillFoldResult) { r.Candidate.FilledQuantity.ProtoReflect().SetUnknown([]byte{0x78, 1}) }, "failure with state": func(r *omspb.FillFoldResult) { r.Status = omspb.FillFoldStatus_FILL_FOLD_STATUS_REJECTED }, "unknown status": func(r *omspb.FillFoldResult) { r.Status = 99 }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := response(j, c)
			mutate(r)
			if s := compare(j, c, r, "stamp"); s != "invalid_result" {
				t.Fatal(s)
			}
		})
	}
	r := response(j, c)
	r.Candidate.OrderId = "other"
	if s := compare(j, c, r, "stamp"); s != "disagreement" {
		t.Fatal(s)
	}
	c.Tenant = "other"
	if _, e := job(c, "tenant", "go"); e == nil {
		t.Fatal("cross-tenant capture accepted")
	}
	c.Tenant = "tenant"
	c.Fill.Price.ProtoReflect().SetUnknown([]byte{0x78, 1})
	if _, e := job(c, "tenant", "go"); e == nil {
		t.Fatal("unknown input fields silently lost")
	}
}
func TestQueueDetachedAndBounded(t *testing.T) {
	c := fixture(t)
	statuses := []string{}
	w := &Worker{done: make(chan struct{}), queue: make(chan order.FillFoldCapture, 1), observe: func(s string) { statuses = append(statuses, s) }}
	w.Submit(c)
	c.State.OrderId = "mutated"
	c.Fill.Quantity.Coefficient = 9
	c.Candidate.OrderId = "mutated"
	queued := <-w.queue
	if queued.State.OrderId != "order" || queued.Fill.Quantity.Coefficient != 2 || queued.Candidate.OrderId != "order" {
		t.Fatal("queue aliases caller")
	}
	w.queue <- queued
	w.Submit(c)
	if len(statuses) != 1 || statuses[0] != "queue_full" {
		t.Fatal(statuses)
	}
	c.State.ProtoReflect().SetUnknown(make([]byte, 1<<20))
	w.Submit(c)
	if statuses[len(statuses)-1] != "invalid_input" {
		t.Fatal(statuses)
	}
	if worker, e := New(context.Background(), Config{}, "", "", nil); e != nil || worker != nil {
		t.Fatal("default enabled")
	}
	for _, cfg := range []Config{{Executable: "partial"}, {ExpectedVersion: "partial"}, {Enabled: true}, {Enabled: true, Executable: "relative", ExpectedVersion: "stamp", Timeout: time.Second}} {
		if cfg.Validate() == nil {
			t.Fatal("invalid config accepted")
		}
	}
}
func TestRustOMSFillShadowActualProcess(t *testing.T) {
	exe := os.Getenv("KANZ_OMS_FOLD_BIN")
	if exe == "" {
		t.Skip("real Rust OMS fold binary not configured")
	}
	cfg := Config{Enabled: true, Executable: exe, ExpectedVersion: os.Getenv("KANZ_OMS_FOLD_VERSION"), Timeout: 5 * time.Second}
	if e := cfg.Validate(); e != nil {
		t.Fatal(e)
	}
	c := fixture(t)
	if s := evaluate(context.Background(), cfg, "tenant", "go-test", c); s != "matched" {
		t.Fatal(s)
	}
	c.State.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
	c.Candidate = nil
	c.RejectionCode = "ORDER_TERMINAL"
	if s := evaluate(context.Background(), cfg, "tenant", "go-test", c); s != "matched" {
		t.Fatal(s)
	}
	cfg.ExpectedVersion = "wrong"
	if s := evaluate(context.Background(), cfg, "tenant", "go-test", c); s != "invalid_result" {
		t.Fatal(s)
	}
}
