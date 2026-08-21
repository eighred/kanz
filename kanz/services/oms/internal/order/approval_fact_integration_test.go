package order

// THE APPROVAL FACT, OVER A REAL SPINE (#410, acceptance clause (d)).
//
// approval_fact_test.go proves the OMS builds and emits ORDER_APPROVED carrying
// both names. It proves it against fakeBus, which does NOT run bus.Validate —
// and this repository's own history is that TWELVE publish sites "validated fine
// in unit tests and failed on the first real broker" (pkg/bus/producer.go), and
// that a lenient double let the OMS ship a publish with no tenant that a live
// JetStream rejected and that crash-looped the service. A double that accepts
// what the broker rejects certifies nothing.
//
// The FACT that answers "who countersigned this order" is the last thing that
// should be provable only against a double. So this drives the whole path on a
// real broker:
//
//	an armed OMS holds a large order          →  ORDER_PENDING_APPROVAL
//	a DIFFERENT authenticated subject signs   →  the claim commits with its FACT
//	the outbox relay publishes it             →  ORDER_APPROVED round-trips through
//	                                             JetStream and is read back with
//	                                             BOTH names on it
//
// Non-vacuity is not optional: the same rig also runs a SELF-approval and
// asserts that NO ORDER_APPROVED appears for it. A build that announced an
// approval unconditionally would satisfy the headline assertion while recording
// four-eyes on orders one person signed alone.

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/approval"

	"github.com/prometheus/client_golang/prometheus"
)

// approvalCollector records the ORDER_APPROVED FACTs that come BACK off the
// broker, keyed by order id. It is fed by a real bus.Consumer, so every envelope
// it sees has already passed bus.Validate on the receive side — which is the
// half a captured-Event double can never assert.
type approvalCollector struct {
	mu sync.Mutex
	by map[string][]*orderpb.OrderApproved
}

func (c *approvalCollector) handle(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
	var ap orderpb.OrderApproved
	if err := proto.Unmarshal(payload, &ap); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.by[ap.GetOrderId()] = append(c.by[ap.GetOrderId()], &ap)
	return nil
}

func (c *approvalCollector) get(orderID string) []*orderpb.OrderApproved {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*orderpb.OrderApproved(nil), c.by[orderID]...)
}

// total is how many ORDER_APPROVED FACTs arrived for ANY order. It is what makes
// the non-vacuity check below sound rather than a race: a refusal that wrongly
// announced would land here even under an order id the test did not think to
// look up.
func (c *approvalCollector) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.by {
		n += len(v)
	}
	return n
}

func TestAnApprovedOrderAnnouncesBothIdentitiesOverARealSpine(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the order-approval FACT over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenant := "oms-approve-it-" + suffix

	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	// order.> is where BOTH halves have to land — the hold's
	// ORDER_PENDING_APPROVAL and the approval's ORDER_APPROVED. A JetStream
	// publish to an unbound subject is a hard error, which is exactly the failure
	// a missing grant or a missing stream mapping produces in a cluster and which
	// no unit test can see.
	bustest.EnsureSubjects(t, ctx, js, "ORDER_OMS_APPROVE_IT_"+suffix, []string{"order.>"})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "oms-approve-it"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	producer, err := bus.NewProducer(client, bus.ProducerConfig{
		Source: "oms", ProducerVersion: "it", Tenant: tenant,
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatal(err)
	}

	collector := &approvalCollector{by: map[string][]*orderpb.OrderApproved{}}
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go func() {
		_ = consumer.Subscribe(watchCtx, EventTypeApproved, "oms-approve-it-"+suffix, collector.handle)
	}()

	// --- a real OMS with the dual-control gate ARMED --------------------------
	gate, err := approval.NewGate(true, dualRat("1000"), nil, prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewGate armed: %v", err)
	}
	store := NewMemoryStore()
	svc, err := NewService(tenant, store, NewEmitter(producer), nil, nil, nil, nil,
		WithDualControl(gate),
		WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// The envelope and ctx a real delivery would carry: the api-gateway stamps
	// the authenticated caller's tenant, and the OMS publishes under it. An
	// approval arriving with no tenant is the shape that crash-looped this
	// service, and outbox.From refuses it rather than enqueuing a FACT that could
	// never be published.
	handlerCtx := bus.WithTenantID(ctx, tenant)
	submit := &envelopepb.Envelope{EventType: SubjectSubmit, TenantId: tenant}
	approve := &envelopepb.Envelope{EventType: SubjectApprove, TenantId: tenant}

	hold := func(orderID, proposer string) *orderpb.SubmitOrder {
		t.Helper()
		cmd := largeOrderFrom(proposer)
		cmd.OrderId = orderID
		cmd.Metadata.TargetId = orderID
		if err := svc.Handle(handlerCtx, submit, mustMarshal(t, cmd)); err != nil {
			t.Fatalf("Handle(submit %s): %v", orderID, err)
		}
		if _, _, err := store.Load(ctx, orderID); err == nil {
			t.Fatalf("setup: %s was ADMITTED rather than held — the gate is not armed, and every "+
				"assertion below would be about an order nobody had to sign", orderID)
		}
		return cmd
	}

	// --- NON-VACUITY FIRST, AND THE ORDER IS THE WHOLE POINT ------------------
	//
	// The refused approval is driven BEFORE the accepted one so that any FACT it
	// wrongly published is already on the stream when the accepted one arrives.
	// Both land on the same subject through the same consumer, so waiting for the
	// second is a barrier for the first. Run the other way round, the absence
	// check would be a race the collector simply had not caught up with — and it
	// would pass against a build that announced every refusal.
	selfID := "o-self-" + suffix
	self := hold(selfID, "user:carol@kanz")
	if err := svc.Handle(handlerCtx, approve, mustMarshal(t, approvalOf(t, self, "user:carol@kanz"))); err != nil {
		t.Fatalf("Handle(self-approve): %v", err)
	}

	// --- THE INVARIANT: a second subject signs, and the bus is told who --------
	signedID := "o-signed-" + suffix
	signed := hold(signedID, "user:alice@kanz")
	if err := svc.Handle(handlerCtx, approve, mustMarshal(t, approvalOf(t, signed, "user:bob@kanz"))); err != nil {
		t.Fatalf("Handle(approve): %v", err)
	}
	if _, _, err := store.Load(ctx, signedID); err != nil {
		t.Fatalf("the approved order is not in the store (%v) — the release did not happen, so "+
			"what follows would prove nothing about a released order", err)
	}

	// --- the approval reached the broker and came back valid ------------------
	var got []*orderpb.OrderApproved
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if got = collector.get(signedID); len(got) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(got) == 0 {
		t.Fatal("the ORDER_APPROVED FACT never came back off the broker. The order is live and " +
			"services/audit — which folds bus FACTs and cannot read the OMS's tables — has no " +
			"way to answer who countersigned it.")
	}
	if len(got) != 1 {
		t.Fatalf("one decision produced %d ORDER_APPROVED FACTs", len(got))
	}
	ap := got[0]
	if ap.GetProposer() != "user:alice@kanz" {
		t.Errorf("proposer = %q off the broker, want user:alice@kanz — a FACT naming only the "+
			"approver is compatible with one person holding both signatures", ap.GetProposer())
	}
	if ap.GetApprover() != "user:bob@kanz" {
		t.Errorf("approver = %q off the broker, want user:bob@kanz", ap.GetApprover())
	}
	if ap.GetAct() != string(dualcontrol.ActOrderSubmission) {
		t.Errorf("act = %q, want %q", ap.GetAct(), dualcontrol.ActOrderSubmission)
	}
	if ap.GetDigest() == "" {
		t.Error("digest is empty off the broker — nothing ties this approval to the order that " +
			"was proposed, so a consumer cannot prove the order that traded is the order signed")
	}
	if ap.GetApprovedAt().AsTime().IsZero() {
		t.Error("approved_at is unset off the broker")
	}

	// THE REFUSED ONE ANNOUNCED NOTHING. It was driven first, so it has had every
	// chance to arrive; the accepted FACT landing is the barrier that proves the
	// stream was drained past it.
	if bad := collector.get(selfID); len(bad) > 0 {
		t.Fatalf("a SELF-APPROVAL was announced on the bus as an approval carrying %q/%q — the "+
			"audit trail now shows four-eyes on an order one person signed alone",
			bad[0].GetProposer(), bad[0].GetApprover())
	}
	if n := collector.total(); n != 1 {
		t.Fatalf("%d ORDER_APPROVED FACTs reached the broker in a run with ONE real approval — "+
			"a control that announces approvals nobody gave is worse than the silence it replaced", n)
	}
}
