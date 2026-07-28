package posttrade_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	observationpb "github.com/kanz-eng/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/posttrade"
)

// captureClient records every Publish (the EVT-17 / integrity test seam).
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

// fakeEncoder stands in for the composition-root settlement.v1.SettlementFail
// builder — settlement.v1 is generated-not-committed, so the test uses an
// already-generated proto to exercise the emitter's enveloping/publish path.
func fakeEncoder(f posttrade.Fail) (proto.Message, error) {
	return &observationpb.DecisionLog{Summary: f.Reason, Attributes: map[string]string{"instruction": f.InstructionID}}, nil
}

func newBusSink(t *testing.T, enc posttrade.FailEncoder) (*posttrade.BusFailSink, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "oms/test", ProducerVersion: "oms-1.0.0", Tenant: "acme"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	sink, err := posttrade.NewBusFailSinkWithClock(prod, enc, func() time.Time { return time.Unix(1000, 0) })
	if err != nil {
		t.Fatalf("NewBusFailSink: %v", err)
	}
	return sink, cc
}

func TestBusFailSinkPublishesSettlementFailFact(t *testing.T) {
	sink, cc := newBusSink(t, fakeEncoder)
	f := posttrade.Fail{InstructionID: "i1", Severity: posttrade.SeverityCritical, Reason: "unsettled 3 days"}
	if err := sink.Publish(context.Background(), f); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(cc.sent) != 1 {
		t.Fatalf("captured %d want 1", len(cc.sent))
	}
	var frame envelopepb.EventFrame
	if err := proto.Unmarshal(cc.sent[0].Body, &frame); err != nil {
		t.Fatalf("frame unmarshal: %v", err)
	}
	if err := bus.Validate(frame.Envelope); err != nil {
		t.Fatalf("envelope invalid: %v", err)
	}
	env := frame.Envelope
	if env.EventType != "settlement.instruction.fail" || env.Domain != "settlement" {
		t.Errorf("event_type/domain = %q/%q want settlement.instruction.fail/settlement", env.EventType, env.Domain)
	}
	if env.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("class = %v want FACT", env.EventClass)
	}
	if env.PartitionKey != "i1" {
		t.Errorf("partition_key = %q want i1 (instruction id)", env.PartitionKey)
	}
}

// EmitFails drives the concrete sink over a batch of detected fails.
func TestEmitFailsOverBusSink(t *testing.T) {
	sink, cc := newBusSink(t, fakeEncoder)
	fails := []posttrade.Fail{{InstructionID: "i1"}, {InstructionID: "i2"}}
	n, err := posttrade.EmitFails(context.Background(), sink, fails)
	if err != nil || n != 2 {
		t.Fatalf("EmitFails = (%d,%v) want (2,nil)", n, err)
	}
	if len(cc.sent) != 2 {
		t.Errorf("published %d want 2", len(cc.sent))
	}
}

// An encoder error aborts the publish (nothing is emitted for that fail).
func TestBusFailSinkEncoderError(t *testing.T) {
	sink, cc := newBusSink(t, func(posttrade.Fail) (proto.Message, error) {
		return nil, errors.New("encode boom")
	})
	if err := sink.Publish(context.Background(), posttrade.Fail{InstructionID: "i1"}); err == nil {
		t.Fatal("expected an encoder error to propagate")
	}
	if len(cc.sent) != 0 {
		t.Errorf("nothing should be published on encode error, got %d", len(cc.sent))
	}
}

func TestNewBusFailSinkNilArgs(t *testing.T) {
	if _, err := posttrade.NewBusFailSink(nil, fakeEncoder); err == nil {
		t.Error("nil producer should error")
	}
	prod, _ := bus.NewProducer(&captureClient{}, bus.ProducerConfig{Source: "s", ProducerVersion: "v", Tenant: "acme"})
	if _, err := posttrade.NewBusFailSink(prod, nil); err == nil {
		t.Error("nil encoder should error")
	}
}
