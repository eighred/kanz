package order

// THE GATE BLOCKING AN ORDER, OVER A REAL SPINE (#245).
//
// internal/compliance/mandate_arming_test.go proves a restarted gate ARMS
// itself. Arming is the precondition; refusing is the control. Everything
// between them — a real mandate decoded off the bus, a real Engine evaluating a
// real order, and the ORDER_REJECTED FACT that tells the caller and every
// downstream fold — was proven only by denyGate{} in service_test.go, a stub
// that returns a Breach unconditionally. A stub that always denies cannot show
// that the mandate was read, that the right rule fired, or that the refusal is
// publishable: the OMS's own history is that a FACT which passed every unit test
// was rejected by the broker on the first live publish.
//
// So this drives the whole chain on the real broker:
//
//	operator publishes a DENY mandate  →  MandateConsumer arms the registry
//	→  PreTradeGate + Engine evaluate a real SubmitOrder
//	→  COMP01Gate maps the breach  →  Service.refuse emits ORDER_REJECTED
//	→  the FACT round-trips through JetStream and passes bus.Validate on receive.
//
// Non-vacuity is not optional here. A gate that refused EVERYTHING would satisfy
// the denial assertion, and that is the exact shape of a broken control, so the
// same rig admits a permitted instrument in the same run.

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/bustest"
	comp "github.com/eighred/kanz/internal/compliance"
	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/pkg/bus"
	omscompliance "github.com/eighred/kanz/services/oms/internal/compliance"
)

// rejectionCollector records the ORDER_REJECTED FACTs that come BACK off the
// broker, keyed by order id. It is fed by a real bus.Consumer, so every envelope
// it sees has already passed bus.Validate on the receive side — which is the
// half a captured-Event double can never assert.
type rejectionCollector struct {
	mu sync.Mutex
	by map[string]*orderpb.OrderRejected
}

func (c *rejectionCollector) handle(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
	var rej orderpb.OrderRejected
	if err := proto.Unmarshal(payload, &rej); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.by[rej.GetOrderId()] = &rej
	return nil
}

func (c *rejectionCollector) get(orderID string) *orderpb.OrderRejected {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.by[orderID]
}

func TestAMandateRefusesAnOrderOverARealSpine(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL to drive the pre-trade compliance refusal over a real spine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	// UNIQUE per run: MANDATE is compacted and never ages out, so a previous
	// run's mandate would arm this one and hide a registry that learned nothing.
	tenant := "oms-comp-it-" + suffix
	portfolio := "pf-" + suffix
	forbidden, permitted := "ETH-USD-"+suffix, "BTC-USD-"+suffix

	admin, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	// Both halves of the chain must be carried by a stream. compliance.mandate.>
	// feeding the gate is exactly the binding whose absence took the OMS's whole
	// consumer group down (infra/nats/bootstrap-job.yaml), and order.> is
	// where the refusal has to land.
	bustest.EnsureSubjects(t, ctx, js, "MANDATE_OMS_IT_"+suffix, []string{comp.SubjectMandateAll})
	bustest.EnsureSubjects(t, ctx, js, "ORDER_OMS_IT_"+suffix, []string{"order.>"})

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "oms-compliance-it"})
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

	// --- the operator puts the portfolio under a mandate, over the bus ---------
	mandate := &compliancepb.Mandate{
		MandateId:   "m-" + portfolio,
		TenantId:    tenant,
		PortfolioId: portfolio,
		Version:     1,
		EffectiveAt: timestamppb.New(time.Now().Add(-time.Hour).UTC()),
		Rules: []*compliancepb.Rule{{
			RuleId: "no-eth",
			Type:   compliancepb.RuleType_RULE_TYPE_RESTRICTION,
			Params: &compliancepb.Rule_Restriction{Restriction: &compliancepb.RestrictionList{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				Mode:      compliancepb.RestrictionMode_RESTRICTION_MODE_DENY,
				Values:    []string{forbidden},
			}},
		}},
	}
	// A REAL TWO-PERSON APPROVAL. The publisher takes no lone actor since #410, so
	// even a fixture has to go through the path production goes through — which is
	// the point: a test that could publish unilaterally would be testing a
	// signature that no longer exists.
	mandateDigest, err := comp.MandateDigest(mandate, "#245")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	prop, err := dualcontrol.Propose("p-245", dualcontrol.ActMandateChange, "mandate",
		"operator:proposer", mandateDigest, now, dualcontrol.DefaultTTL)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := prop.Approve("operator:approver", mandateDigest, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := comp.NewPublisher(producer).Publish(ctx, mandate, nil, approval, "#245"); err != nil {
		t.Fatalf("publish mandate: %v", err)
	}

	// --- the gate arms itself from the spine ----------------------------------
	registry := comp.NewMandateRegistry()
	mandateConsumer := comp.NewMandateConsumer(registry, nil)
	armCtx, stopArm := context.WithCancel(ctx)
	defer stopArm()
	go func() {
		_ = consumer.SubscribeBroadcast(armCtx, comp.SubjectMandateAll, mandateConsumer.Handle)
	}()

	armed := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if _, ok, _ := registry.Mandate(ctx, tenant, portfolio, time.Now()); ok {
			armed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !armed {
		t.Fatal("the registry never received the mandate off the spine — the gate is unarmed, " +
			"and an unarmed gate PASSES every order")
	}

	// --- a real OMS, with the real gate and a real bus producer ---------------
	preTrade := comp.NewPreTradeGate(comp.NewEngine(nil), comp.MapBookSource{}, registry, nil, nil, nil)
	gate := omscompliance.NewCOMP01Gate(preTrade, "USD")

	store := NewMemoryStore()
	svc, err := NewService(tenant, store, NewEmitter(producer), gate, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Watch for the refusal FACT coming back off the broker.
	collector := &rejectionCollector{by: map[string]*orderpb.OrderRejected{}}
	rejCtx, stopRej := context.WithCancel(ctx)
	defer stopRej()
	go func() {
		_ = consumer.Subscribe(rejCtx, EventTypeRejected, "oms-comp-it-"+suffix, collector.handle)
	}()

	// The envelope a real delivery would carry: the api-gateway stamps the
	// authenticated caller's tenant, and the gate looks the mandate up under it
	// rather than under this OMS's serving tenant (#243).
	submitEnvelope := &envelopepb.Envelope{EventType: SubjectSubmit, TenantId: tenant}
	order := func(orderID, instrument string) *orderpb.SubmitOrder {
		return &orderpb.SubmitOrder{
			OrderId:      orderID,
			PortfolioId:  portfolio,
			InstrumentId: instrument,
			Side:         orderpb.Side_SIDE_BUY,
			Quantity:     d(10, 0),
			OrderType:    orderpb.OrderType_ORDER_TYPE_LIMIT,
			LimitPrice:   d(100, 0),
			TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_DAY,
			Metadata:     &commandpb.CommandMetadata{Issuer: "service:test"},
		}
	}

	// --- THE INVARIANT: the forbidden order is REFUSED ------------------------
	deniedID := "o-denied-" + suffix
	handlerCtx := bus.WithTenantID(ctx, tenant)
	if err := svc.Handle(handlerCtx, submitEnvelope, mustMarshal(t, order(deniedID, forbidden))); err != nil {
		t.Fatalf("Handle (denied order): %v", err)
	}
	if _, _, err := store.Load(ctx, deniedID); err == nil {
		t.Fatal("the refused order was PERSISTED — the gate must refuse before admission, " +
			"or a mandate-breaching order exists in the book and can be worked")
	}

	// --- NON-VACUITY: the permitted order is admitted in the same run ---------
	allowedID := "o-allowed-" + suffix
	if err := svc.Handle(handlerCtx, submitEnvelope, mustMarshal(t, order(allowedID, permitted))); err != nil {
		t.Fatalf("Handle (permitted order): %v", err)
	}
	if _, _, err := store.Load(ctx, allowedID); err != nil {
		t.Fatalf("an instrument the mandate PERMITS was also refused (%v) — a gate that denies "+
			"everything is not a control, and it would have satisfied the assertion above", err)
	}

	// --- the refusal reached the broker and came back valid -------------------
	var rej *orderpb.OrderRejected
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if rej = collector.get(deniedID); rej != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rej == nil {
		t.Fatal("the ORDER_REJECTED FACT never came back off the broker. The caller was told " +
			"nothing and no downstream fold saw the refusal — the order is simply missing.")
	}
	if rej.GetErrorCode() != "COMPLIANCE_RESTRICTION" {
		t.Fatalf("rejection error_code = %q, want COMPLIANCE_RESTRICTION — the refusal must name "+
			"the rule that fired, not merely that something did", rej.GetErrorCode())
	}
	if collector.get(allowedID) != nil {
		t.Fatal("a rejection FACT was emitted for the PERMITTED order")
	}
}
