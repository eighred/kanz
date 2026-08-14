package costwatch_test

// THE ENVELOPE, NOT THE ENCODER (#245, applied to #436).
//
// The tests in costwatch_test.go publish through a fake Bus at the EVENT level.
// That proves the fields this package sets, and nothing about whether a real
// broker would accept the envelope — which is exactly the shape
// pkg/bus/producer.go's header records: "TWELVE publish sites … validated fine in
// unit tests (which inject fake Publishers that never validate) and failed on the
// first real broker."
//
// CLAUDE.md says the same thing as a constraint: "fakeBus does not validate
// envelopes, so it accepts what a real broker rejects. A green suite using it is
// not a broker proof."
//
// These are Tier-B: a REAL bus.Producer over a fake bus.Client. The fake sits at
// the TRANSPORT level — it receives wire bytes — so stamping, Validate and
// framing all really run, and the only thing mocked out is the socket. A double
// at the Event level would prove nothing here, because Validate is precisely what
// it skips.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/costwatch"
)

// captureClient is a bus.Client that records the framed wire bytes. Same helper
// shape internal/risk/publish and services/accounting/internal/cashmove use, and
// deliberately BELOW the Producer so Validate is on the path.
type captureClient struct {
	mu   sync.Mutex
	sent []bus.Message
}

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}

func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}

func (c *captureClient) Close() error { return nil }

func (c *captureClient) messages() []bus.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bus.Message(nil), c.sent...)
}

func d(coef int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coef, Exponent: exp}
}

// filledFact is the inbound FACT: a fill plus the order state carrying the
// arrival mark the measurement is taken against.
func filledFact(t *testing.T) []byte {
	t.Helper()
	b, err := proto.Marshal(&orderpb.OrderFilled{
		OrderId: "o-1",
		Fill: &orderpb.Fill{
			FillId: "f-1", OrderId: "o-1", InstrumentId: "BTC-USD", Venue: "XBIN",
			Side: orderpb.Side_SIDE_BUY, Quantity: d(10, 0), Price: d(110, 0),
			Fee: &commonpb.Money{Amount: d(2, 0), CurrencyCode: "USD"},
		},
		State: &orderpb.OrderState{
			OrderId: "o-1", InstrumentId: "BTC-USD", Venue: "XBIN",
			Side: orderpb.Side_SIDE_BUY, ArrivalPrice: d(100, 0),
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// A COST RECORD MUST BE AN ENVELOPE A REAL BROKER ACCEPTS.
//
// Every field asserted below is one bus.Validate rejects when absent. A missing
// payload_schema_ref or partition_key is not a degraded FACT, it is a DROPPED
// one — and a dropped cost record is invisible in exactly the way that matters:
// the metric still moves, the dashboard still looks populated, and the durable
// series a venue ranking reads is quietly short.
func TestCostRecordIsAValidFactEnvelope(t *testing.T) {
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source: "oms", ProducerVersion: "test", Tenant: "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	w := costwatch.New(prometheus.NewRegistry(), "acme", prod, nil, slog.New(slog.DiscardHandler))

	err = w.Handle(context.Background(),
		&envelopepb.Envelope{EventId: "e-1", EventType: costwatch.EventTypeFilled, TenantId: "acme"},
		filledFact(t))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	msgs := cc.messages()
	if len(msgs) != 1 {
		t.Fatalf("published %d messages, want 1 — nothing reached the transport, so Validate "+
			"never ran and this test proves nothing", len(msgs))
	}

	env, payload, err := bus.Unframe(msgs[0].Body)
	if err != nil {
		t.Fatalf("the published bytes do not unframe: %v", err)
	}

	// It passed Validate by reaching the transport at all. These assert the
	// fields a consumer depends on, which Validate checks for presence and this
	// checks for correctness.
	if env.GetEventType() != costwatch.EventTypeCostRecorded {
		t.Errorf("event_type = %q, want %q", env.GetEventType(), costwatch.EventTypeCostRecorded)
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event_class = %v, want FACT", env.GetEventClass())
	}
	if env.GetTenantId() != "acme" {
		t.Errorf("tenant_id = %q, want acme — an untenanted FACT is refused by the broker",
			env.GetTenantId())
	}
	if env.GetPartitionKey() != "o-1" {
		t.Errorf("partition_key = %q, want the ORDER id, so an order's fills stay in sequence "+
			"on one partition", env.GetPartitionKey())
	}
	if env.GetPayloadSchemaRef() == "" {
		t.Error("no payload_schema_ref — a consumer cannot tell which version it is decoding")
	}

	var rec orderpb.TransactionCostRecorded
	if err := proto.Unmarshal(payload, &rec); err != nil {
		t.Fatalf("payload does not decode as TransactionCostRecorded: %v", err)
	}
	if rec.GetVenue() != "XBIN" || rec.GetFillId() != "f-1" {
		t.Errorf("payload lost its identity: venue=%q fill=%q", rec.GetVenue(), rec.GetFillId())
	}
	// The fee travelled with its CURRENCY. An amount without one is a number
	// nobody can add to another venue's.
	if rec.GetFee().GetCurrencyCode() != "USD" {
		t.Errorf("fee currency = %q, want USD", rec.GetFee().GetCurrencyCode())
	}
}
