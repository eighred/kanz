package foldshadow

import (
	"context"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	omspb "github.com/eighred/kanz/kanz-schemas-go/oms/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/protobuf/proto"
	"os"
	"testing"
	"time"
)

func terminalFixture(t *testing.T, op string) order.TerminalFoldCapture {
	t.Helper()
	base := fixture(t)
	var candidate *orderpb.OrderState
	var quantity *commonpb.Decimal
	var err error
	if op == "cancel" {
		candidate, quantity, err = order.Cancel(base.State, base.EventTime)
	} else {
		candidate, quantity, err = order.Expire(base.State, base.EventTime)
	}
	if err != nil {
		t.Fatal(err)
	}
	return order.TerminalFoldCapture{Tenant: base.Tenant, Path: "reconcile", Operation: op, ExpectedVersion: base.ExpectedVersion, EventTime: base.EventTime, CapturedAt: base.CapturedAt, State: base.State, Candidate: candidate, UnfilledQuantity: quantity}
}
func terminalResponse(j *omspb.TerminalFoldRequest, c order.TerminalFoldCapture) *omspb.TerminalFoldResult {
	return &omspb.TerminalFoldResult{CaptureId: j.CaptureId, CaptureDigest: j.CaptureDigest, TenantId: j.TenantId, ProducerVersion: "stamp", ExpectedVersion: j.ExpectedVersion, Operation: j.Operation, Status: omspb.TerminalFoldStatus_TERMINAL_FOLD_STATUS_OK, Candidate: proto.Clone(c.Candidate).(*orderpb.OrderState), UnfilledQuantity: proto.Clone(c.UnfilledQuantity).(*commonpb.Decimal)}
}
func TestTerminalResultBindingAndPresence(t *testing.T) {
	c := terminalFixture(t, "cancel")
	c.ExpectedVersion = 0 // Newly admitted pending orders can be cancelled at SQL version zero.
	j, err := terminalJob(c, "tenant", "source")
	if err != nil {
		t.Fatal(err)
	}
	if compareTerminal(j, c, terminalResponse(j, c), "stamp") != "matched" {
		t.Fatal("baseline not matched")
	}
	for name, mutate := range map[string]func(*omspb.TerminalFoldResult){
		"operation": func(r *omspb.TerminalFoldResult) {
			r.Operation = omspb.TerminalFoldOperation_TERMINAL_FOLD_OPERATION_EXPIRE
		}, "tenant": func(r *omspb.TerminalFoldResult) { r.TenantId = "other" }, "digest": func(r *omspb.TerminalFoldResult) { r.CaptureDigest = nil }, "stamp": func(r *omspb.TerminalFoldResult) { r.ProducerVersion = "wrong" }, "version": func(r *omspb.TerminalFoldResult) { r.ExpectedVersion = nil }, "capture": func(r *omspb.TerminalFoldResult) { r.CaptureId = "wrong" }, "quantity": func(r *omspb.TerminalFoldResult) { r.UnfilledQuantity = nil }, "state": func(r *omspb.TerminalFoldResult) { r.Candidate.PortfolioId = "wrong" }, "nested-unknown": func(r *omspb.TerminalFoldResult) { r.UnfilledQuantity.ProtoReflect().SetUnknown([]byte{0x78, 1}) }, "unknown-status": func(r *omspb.TerminalFoldResult) { r.Status = 99 },
	} {
		t.Run(name, func(t *testing.T) {
			r := terminalResponse(j, c)
			mutate(r)
			if compareTerminal(j, c, r, "stamp") == "matched" {
				t.Fatal("changed result matched")
			}
		})
	}
	c.UnfilledQuantity = nil
	r := terminalResponse(j, terminalFixture(t, "cancel"))
	r.UnfilledQuantity = &commonpb.Decimal{}
	if compareTerminal(j, c, r, "stamp") != "disagreement" {
		t.Fatal("absence became zero")
	}
	c.Tenant = "other"
	if _, err := terminalJob(c, "tenant", "source"); err == nil {
		t.Fatal("cross tenant input")
	}
}
func TestTerminalSharesBoundedQueue(t *testing.T) {
	var statuses []string
	w := &Worker{done: make(chan struct{}), queue: make(chan capturedWork, 1), observe: func(s string) { statuses = append(statuses, s) }}
	c := terminalFixture(t, "cancel")
	w.SubmitTerminal(c)
	c.State.OrderId = "changed"
	c.Candidate.OrderId = "changed"
	c.UnfilledQuantity.Coefficient = 999
	item := <-w.queue
	if item.terminal.State.OrderId == "changed" || item.terminal.Candidate.OrderId == "changed" || item.terminal.UnfilledQuantity.Coefficient == 999 {
		t.Fatal("queued capture aliases")
	}
	w.queue <- item
	w.Submit(fixture(t))
	if len(statuses) != 1 || statuses[0] != "queue_full" {
		t.Fatal("fill and terminal queues are not one bound")
	}
	c.State.ProtoReflect().SetUnknown(make([]byte, 1<<20))
	w.SubmitTerminal(c)
	if statuses[len(statuses)-1] != "invalid_input" {
		t.Fatal("oversize capture queued")
	}
	close(w.done)
	before := len(statuses)
	w.SubmitTerminal(c)
	if len(statuses) != before {
		t.Fatal("closed worker accepted capture")
	}
}
func TestRustOMSTerminalShadowActualProcess(t *testing.T) {
	bin := os.Getenv("KANZ_OMS_FOLD_BIN")
	if bin == "" {
		t.Skip("actual Rust terminal binary unavailable")
	}
	cfg := Config{Enabled: true, Executable: bin, ExpectedVersion: os.Getenv("KANZ_OMS_FOLD_VERSION"), Timeout: 5 * time.Second}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"cancel", "expire"} {
		c := terminalFixture(t, op)
		if status := evaluateTerminal(context.Background(), cfg, "tenant", "source", c); status != "matched" {
			t.Fatal(op, status)
		}
		c.State.Status = orderpb.OrderStatus_ORDER_STATUS_FILLED
		c.Candidate = nil
		c.UnfilledQuantity = nil
		c.RejectionCode = "ORDER_TERMINAL"
		if status := evaluateTerminal(context.Background(), cfg, "tenant", "source", c); status != "matched" {
			t.Fatal("refusal", status)
		}
	}
	cfg.ExpectedVersion = "wrong"
	if status := evaluateTerminal(context.Background(), cfg, "tenant", "source", terminalFixture(t, "cancel")); status != "invalid_result" {
		t.Fatal("unstamped result accepted", status)
	}
}
