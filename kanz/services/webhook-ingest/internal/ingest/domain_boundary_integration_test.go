package ingest

// #95 — AN OUT-OF-DOMAIN DECIMAL IS REFUSED AT THE BUS BOUNDARY, AGAINST A REAL BROKER.
//
// The code half of #95 shipped in #184: every bus ingress calls dec.InDomainDeep
// on the decoded message before any Decimal reaches dec.FromProto. What could not
// be shown was this issue's own wording — "proven against a real broker" —
// because fakeBus does not validate envelopes and therefore accepts what a real
// broker rejects (AGENTS.md), so a green unit suite is explicitly not the proof.
//
// This drives the OMS black-box over a real NATS, exactly as the M1 loop test
// beside it does, and for the same reason: the isolation invariant forbids
// importing the OMS internals in-process.
//
// WHAT THIS TEST IS ACTUALLY FOR, AND WHY "NO ORDER WAS CREATED" IS NOT IT.
//
// The hazard is not a mispriced order. dec.FromProto materialises 10^abs(exponent),
// so an unvalidated wire exponent does not return — it wedges the consumer, and
// "the OMS stops consuming commands entirely" (services/oms/internal/order/service.go).
// A test that only asserted the poison command produced no fill would PASS if the
// OMS had died on it, which is the failure it exists to catch.
//
// So the assertion is LIVENESS: after the poison, a well-formed command receives
// the funded-admission refusal while authoritative cash input is unavailable.
//
// WHAT THIS TEST DOES **NOT** ISOLATE, established by running it against both
// mutations rather than assumed:
//
//	guard present ............ refused at ingress; the OMS logs
//	                           "out-of-domain exponent — refusing" naming limit_price.
//	OMS guard removed ........ STILL REFUSED by compliance's independent decimal
//	                           exponent gate.
//	BOTH removed ............. the OMS WEDGES. The healthy order never receives
//	                           its capital refusal; this test fails on deadline,
//	                           process still burning CPU on 10^2000000000.
//
// So this is a proof of the PROPERTY, not of either guard: two independent layers
// enforce it, and the test cannot tell which one fired. That is worth stating
// plainly rather than claiming a sharper proof than exists — and it is also the
// argument for not "simplifying away" either layer on the grounds that the other
// one covers it. The third row is what makes the test non-vacuous: it can fail,
// and the way it fails is the real defect.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/signal/translate"
	"github.com/eighred/kanz/pkg/bus"
)

// poisonExponent is far outside dec's maxSafeExponent (64). The value is the one
// dec.go's own comment cites: FromProto({Coefficient:1, Exponent:2000000000})
// "does not return within seconds", where exponent 64 is instant. Using the
// documented number keeps this test and that comment describing one fact.
const poisonExponent = 2_000_000_000

func TestIntegration_AnOutOfDomainDecimalIsRefusedAndTheOMSKeepsConsuming(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("set TEST_NATS_URL (with the dev stack + OMS running) to run the bus-boundary refusal proof")
	}
	// BOTH preconditions, for the reason recorded in integration_test.go: a test
	// that gates on NATS alone does not skip when the OMS is absent, it FAILS —
	// and a failure that means "nothing was listening" is indistinguishable here
	// from "the OMS wedged", which is the exact thing under test.
	if os.Getenv("TEST_OMS_ON_BUS") == "" {
		t.Skip("set TEST_OMS_ON_BUS=1 with an OMS consuming order.order.submit on TEST_NATS_URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: "domain-boundary-it"})
	if err != nil {
		t.Fatalf("dial NATS: %v", err)
	}
	defer func() { _ = client.Close() }()

	consumer, err := bus.NewConsumer(client)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	rejections := make(chan *orderpb.OrderRejected, 8)
	go func() {
		_ = consumer.Subscribe(ctx, "order.order.rejected", "domain-boundary-it", func(_ context.Context, _ *envelopepb.Envelope, payload []byte) error {
			var ev orderpb.OrderRejected
			if err := proto.Unmarshal(payload, &ev); err != nil {
				return nil
			}
			rejections <- &ev
			return nil
		})
	}()
	time.Sleep(500 * time.Millisecond) // let the durable bind before publishing

	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "domain-boundary-it", ProducerVersion: "it"})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}

	// FRESH ids per run. The EXECUTION stream carries a 2-minute duplicate window
	// (infra/nats/bootstrap-job.yaml), so fixed ids make a re-run publish commands
	// the broker silently deduplicates — the publish succeeds, nothing lands, and
	// the liveness assertion below would fail for a reason that is not the defect.
	run := time.Now().UnixNano()
	poisonID := fmt.Sprintf("it-poison-%d", run)
	healthyID := fmt.Sprintf("it-healthy-%d", run)

	// The poison differs from the healthy command in ONE field value: the limit
	// price's exponent. Everything else is identical, so a refusal cannot be
	// attributed to a missing field, a bad venue or an unroutable instrument —
	// the domain is the only variable.
	if err := publishSubmit(ctx, producer, poisonID, &commonpb.Decimal{Coefficient: 1, Exponent: poisonExponent}); err != nil {
		t.Fatalf("publish poison command: %v", err)
	}
	if err := publishSubmit(ctx, producer, healthyID, &commonpb.Decimal{Coefficient: 5_000_000_000, Exponent: -8}); err != nil {
		t.Fatalf("publish healthy command: %v", err)
	}

	// LIVENESS. An out-of-domain command is rejected and acknowledged before an
	// OrderState exists, so OMS intentionally emits no OrderRejected for it. The
	// subsequent valid command must still reach the funded admission refusal.
	deadline := time.After(15 * time.Second)
	seen := map[string]string{}
	for {
		if seen[healthyID] != "" {
			break
		}
		select {
		case ev := <-rejections:
			if ev.GetOrderId() == poisonID || ev.GetOrderId() == healthyID {
				seen[ev.GetOrderId()] = ev.GetErrorCode()
			}
		case <-deadline:
			t.Fatalf("OMS did not continue to the next command after malformed exponent: %v", seen)
		}
	}
	if seen[poisonID] != "" && seen[poisonID] == seen[healthyID] || seen[healthyID] != "CAPITAL_TERMS_UNKNOWN" && seen[healthyID] != "CAPITAL_REFUSED" && seen[healthyID] != "CAPITAL_ACTIVATION_REQUIRED" {
		t.Fatalf("domain and capital refusals not distinguished: %v", seen)
	}
}

// publishSubmit emits one SubmitOrder shaped exactly as internal/signal/translate
// emits them, so this exercises the production command path rather than a shape
// invented for a test.
func publishSubmit(ctx context.Context, p *bus.Producer, orderID string, limit *commonpb.Decimal) error {
	cmd := &orderpb.SubmitOrder{
		Metadata:     &commandpb.CommandMetadata{Issuer: "strategy:domain-boundary-it", TargetId: orderID},
		OrderId:      orderID,
		PortfolioId:  "fund-alpha",
		InstrumentId: "BTC-USD",
		Side:         orderpb.Side_SIDE_BUY,
		Quantity:     &commonpb.Decimal{Coefficient: 100_000_000, Exponent: -8}, // 1
		OrderType:    orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:   limit,
		TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		Venue:        "XNAS", // a SimVenue MIC, so a limit order fills deterministically
	}
	return p.Publish(ctx, bus.Event{
		Subject:        translate.SubjectSubmit,
		EventType:      translate.SubjectSubmit,
		EventClass:     envelopepb.EventClass_EVENT_CLASS_COMMAND,
		SchemaVersion:  1,
		Domain:         "order",
		EventTime:      time.Now().UTC(),
		PartitionKey:   orderID,
		IdempotencyKey: orderID,
		TenantID:       "__system__", // matches the OMS_TENANT the harness runs with
		Payload:        cmd,
	})
}
