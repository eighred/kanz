package publish_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	optimizationpb "github.com/eighred/kanz/kanz-schemas-go/optimization/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/optimization/internal/publish"
)

// THESE RUN A REAL bus.Producer OVER A FAKE CLIENT, and that is the whole point.
//
// CLAUDE.md states it plainly: "fakeBus does not validate envelopes, so it
// accepts what a real broker rejects. A green suite using it is not a broker
// proof." Every other test in this package uses such a double — they prove the
// handler's decisions, not the wire. This file proves the envelopes: a real
// producer frames them, and bus.Validate is asserted on the result, so a missing
// tenant, an absent partition key or a wrong event class fails HERE rather than
// on the first live broker.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

type noopCounter struct{}

func (noopCounter) Inc() {}

// newProducer builds the real thing, with NO ProducerConfig.Tenant — exactly as
// the composition root does, and for the reason recorded in
// producersWithoutATenantFallback: the tenant must come from the authenticated
// caller or the publish must fail. A fallback here would make these tests pass
// while hiding the very defect they exist to catch.
func newProducer(t *testing.T) (*bus.Producer, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "optimization/test",
		ProducerVersion: "optimization-1.0.0",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return prod, cc
}

func TestTheOrderCommandEnvelopeSurvivesValidation(t *testing.T) {
	prod, cc := newProducer(t)
	m := publish.NewMaterializer(prod, noopCounter{}, noopCounter{}).ForTenant("acme")

	cmd := &orderpb.SubmitOrder{OrderId: "PF:BTC-USDT:rebal", PortfolioId: "PF"}
	if err := m.Publish(context.Background(), cmd); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(cc.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(cc.sent))
	}

	env, payload, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("a real broker would REJECT this order command: %v", err)
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_COMMAND {
		t.Errorf("event class = %v, want COMMAND", env.GetEventClass())
	}
	if env.GetTenantId() != "acme" {
		t.Errorf("tenant = %q, want the caller's acme", env.GetTenantId())
	}
	var got orderpb.SubmitOrder
	if err := proto.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload is not a SubmitOrder: %v", err)
	}
	if got.GetOrderId() != cmd.GetOrderId() {
		t.Errorf("order id = %q, want %q", got.GetOrderId(), cmd.GetOrderId())
	}
}

func TestTheMaterializedFactEnvelopeSurvivesValidation(t *testing.T) {
	prod, cc := newProducer(t)
	m := publish.NewMaterializer(prod, noopCounter{}, noopCounter{}).ForTenant("acme")

	fact := &optimizationpb.ProposalMaterialized{
		PortfolioId: "PF", Issuer: "alice", Published: true,
		SubmittedOrderIds: []string{"PF:BTC-USDT:rebal"},
	}
	if err := m.Record(context.Background(), fact); err != nil {
		t.Fatalf("Record: %v", err)
	}

	env, payload, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("a real broker would REJECT the audit FACT — the record of who authorized an "+
			"automated capital action would be lost on the live spine: %v", err)
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event class = %v, want FACT", env.GetEventClass())
	}
	var got optimizationpb.ProposalMaterialized
	if err := proto.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload is not a ProposalMaterialized: %v", err)
	}
	if got.GetIssuer() != "alice" || !got.GetPublished() {
		t.Errorf("fact lost its issuer or its published flag on the wire: %+v", &got)
	}
	if got.GetAsOf() == nil {
		t.Error("the FACT carries no as_of — an audit record with no time is not one")
	}
}

// A PUBLISH WITH NO TENANT IS REFUSED BY THE REAL PRODUCER, not merely by this
// package's own guard. Both checks exist on purpose: the local one gives a clear
// error, and this proves the broker would refuse it anyway — so removing the
// local one degrades the message, not the safety.
func TestARealProducerRefusesAnUntenantedEnvelope(t *testing.T) {
	prod, _ := newProducer(t)

	// Bypass Materializer's own tenant check by publishing through the low-level
	// seam with an empty tenant, which is what a fallback-configured producer
	// would have quietly rescued.
	err := publish.NewOrders(prod, "").Publish(context.Background(), &orderpb.SubmitOrder{OrderId: "o1"})
	if err == nil {
		t.Fatal("a real bus.Producer accepted an envelope with no tenant_id. Every assertion in " +
			"this package that relies on the broker refusing one is now unfounded.")
	}
}
