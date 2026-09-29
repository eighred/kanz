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
// AGENTS.md says the same thing as a constraint: "fakeBus does not validate
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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

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

// THE VWAP WINDOW SURVIVES TO THE RECORD.
//
// Implementation shortfall answers "what did the decision cost". Slippage against
// interval VWAP answers the separate question "did we trade worse than the market
// did over the same window" — and an order can beat arrival because the market
// moved in its favour while still being worked worse than every other
// participant. Only the second measure sees that, and it cannot be computed at
// all without the interval.
//
// This record shipped without the window, which is the same defect one layer
// over: a value present upstream and dropped at a boundary, exactly as
// stop_price, expire_at and leverage were.
func TestCostRecordCarriesTheVWAPWindow(t *testing.T) {
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source: "oms", ProducerVersion: "test", Tenant: "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	w := costwatch.New(prometheus.NewRegistry(), "acme", prod, nil, slog.New(slog.DiscardHandler))

	arrival := time.Date(2026, 8, 1, 11, 59, 30, 0, time.UTC)
	executed := time.Date(2026, 8, 1, 12, 0, 15, 0, time.UTC)
	b, err := proto.Marshal(&orderpb.OrderFilled{
		OrderId: "o-1",
		Fill: &orderpb.Fill{
			FillId: "f-1", OrderId: "o-1", InstrumentId: "BTC-USD", Venue: "XBIN",
			Side: orderpb.Side_SIDE_BUY, Quantity: d(10, 0), Price: d(110, 0),
			ExecutedAt: timestamppb.New(executed),
		},
		State: &orderpb.OrderState{
			OrderId: "o-1", InstrumentId: "BTC-USD", Venue: "XBIN",
			Side: orderpb.Side_SIDE_BUY, ArrivalPrice: d(100, 0),
			ArrivalAt: timestamppb.New(arrival),
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := w.Handle(context.Background(),
		&envelopepb.Envelope{EventId: "e-1", EventType: costwatch.EventTypeFilled, TenantId: "acme"},
		b); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	_, payload, err := bus.Unframe(cc.messages()[0].Body)
	if err != nil {
		t.Fatalf("unframe: %v", err)
	}
	var rec orderpb.TransactionCostRecorded
	if err := proto.Unmarshal(payload, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.GetArrivalAt() == nil || !rec.GetArrivalAt().AsTime().Equal(arrival) {
		t.Errorf("arrival_at = %v, want %v — without the window start nothing can select the "+
			"bars this fill should be compared against", rec.GetArrivalAt().AsTime(), arrival)
	}
	if rec.GetExecutedAt() == nil || !rec.GetExecutedAt().AsTime().Equal(executed) {
		t.Errorf("executed_at = %v, want %v", rec.GetExecutedAt().AsTime(), executed)
	}
}

// AN ABSENT WINDOW STAYS ABSENT. An order admitted with no usable mark has no
// arrival_at, and the epoch is not a window — a consumer must skip it rather than
// compute a VWAP from 1970.
func TestCostRecordLeavesAnAbsentWindowUnset(t *testing.T) {
	cc := &captureClient{}
	prod, _ := bus.NewProducer(cc, bus.ProducerConfig{
		Source: "oms", ProducerVersion: "test", Tenant: "acme",
	})
	w := costwatch.New(prometheus.NewRegistry(), "acme", prod, nil, slog.New(slog.DiscardHandler))

	// filledFact carries no ArrivalAt and no ExecutedAt.
	if err := w.Handle(context.Background(),
		&envelopepb.Envelope{EventId: "e-2", EventType: costwatch.EventTypeFilled, TenantId: "acme"},
		filledFact(t)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	_, payload, err := bus.Unframe(cc.messages()[0].Body)
	if err != nil {
		t.Fatalf("unframe: %v", err)
	}
	var rec orderpb.TransactionCostRecorded
	if err := proto.Unmarshal(payload, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.GetArrivalAt() != nil {
		t.Errorf("arrival_at = %v for an order that had none — an epoch timestamp reads as a "+
			"real window and would be joined against 1970's bars", rec.GetArrivalAt().AsTime())
	}
}
