package posttrade_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	settlementpb "github.com/eighred/kanz/kanz-schemas-go/settlement/v1"

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

// validFail is a Fail carrying every field settlement.v1.SettlementFail marks
// Required. The tests used to publish half-populated Fails through a FAKE
// encoder that emitted an observation.DecisionLog, so nothing ever asserted the
// real payload — the emitter's enveloping was covered and its content was not.
// EncodeFail refuses a partial Fail now, so the fixture has to be whole, which
// is the point.
func validFail(id string) posttrade.Fail {
	return posttrade.Fail{
		InstructionID:  id,
		InstrumentID:   "AAPL",
		Counterparty:   "CP-GOLDMAN",
		SettlementDate: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC),
		AgeDays:        3,
		Severity:       posttrade.SeverityCritical,
		Reason:         "unsettled 3 days",
		DetectedAt:     time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}
}

func newBusSink(t *testing.T) (*posttrade.BusFailSink, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "oms/test", ProducerVersion: "oms-1.0.0", Tenant: "acme"})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	sink, err := posttrade.NewBusFailSinkWithClock(prod, func() time.Time { return time.Unix(1000, 0) })
	if err != nil {
		t.Fatalf("NewBusFailSink: %v", err)
	}
	return sink, cc
}

func TestBusFailSinkPublishesSettlementFailFact(t *testing.T) {
	sink, cc := newBusSink(t)
	f := validFail("i1")
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
	if env.PayloadSchemaRef != "settlement.v1.SettlementFail:1" {
		t.Errorf("payload_schema_ref = %q want settlement.v1.SettlementFail:1", env.PayloadSchemaRef)
	}

	// THE PAYLOAD ITSELF, which no test asserted while the encoder was a fake
	// emitting a DecisionLog. AUTO-01 routes on severity and IBOR-01e reconciles
	// on counterparty and settlement_date; an envelope-only assertion would pass
	// with every one of them empty.
	var got settlementpb.SettlementFail
	if err := proto.Unmarshal(frame.Payload, &got); err != nil {
		t.Fatalf("payload unmarshal as settlement.v1.SettlementFail: %v", err)
	}
	if got.GetInstructionId() != "i1" || got.GetInstrumentId() != "AAPL" || got.GetCounterparty() != "CP-GOLDMAN" {
		t.Errorf("identity = %q/%q/%q want i1/AAPL/CP-GOLDMAN",
			got.GetInstructionId(), got.GetInstrumentId(), got.GetCounterparty())
	}
	if got.GetSeverity() != settlementpb.FailSeverity_FAIL_SEVERITY_CRITICAL {
		t.Errorf("severity = %v want CRITICAL — AUTO-01 routes remediation on this", got.GetSeverity())
	}
	if got.GetAgeDays() != 3 {
		t.Errorf("age_days = %d want 3", got.GetAgeDays())
	}
	if got.GetReason() != "unsettled 3 days" {
		t.Errorf("reason = %q", got.GetReason())
	}
	if d := got.GetSettlementDate().AsTime(); !d.Equal(f.SettlementDate) {
		t.Errorf("settlement_date = %v want %v — IBOR-01e reconciles against it", d, f.SettlementDate)
	}
	if d := got.GetDetectedAt().AsTime(); !d.Equal(f.DetectedAt) {
		t.Errorf("detected_at = %v want %v", d, f.DetectedAt)
	}
}

// EmitFails drives the concrete sink over a batch of detected fails.
func TestEmitFailsOverBusSink(t *testing.T) {
	sink, cc := newBusSink(t)
	fails := []posttrade.Fail{validFail("i1"), validFail("i2")}
	n, err := posttrade.EmitFails(context.Background(), sink, fails)
	if err != nil || n != 2 {
		t.Fatalf("EmitFails = (%d,%v) want (2,nil)", n, err)
	}
	if len(cc.sent) != 2 {
		t.Errorf("published %d want 2", len(cc.sent))
	}
}

// AN UNENCODABLE FAIL ABORTS THE PUBLISH, and nothing reaches the wire.
//
// A settlement.v1.SettlementFail with an empty counterparty or a zero settlement
// date is a permanent record nobody can remediate or reconcile against, sitting
// in the SETTLEMENT stream looking like evidence. Refusing at the encoder is
// what keeps it off the stream.
func TestBusFailSinkRefusesAnIncompleteFail(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail posttrade.Fail
		want string
	}{
		{"no instruction id", func() posttrade.Fail { f := validFail("i1"); f.InstructionID = ""; return f }(), "instruction_id"},
		{"no counterparty", func() posttrade.Fail { f := validFail("i1"); f.Counterparty = ""; return f }(), "counterparty"},
		{"zero settlement date", func() posttrade.Fail { f := validFail("i1"); f.SettlementDate = time.Time{}; return f }(), "settlement_date"},
		{"negative age", func() posttrade.Fail { f := validFail("i1"); f.AgeDays = -1; return f }(), "age_days"},
		{"severity none", func() posttrade.Fail { f := validFail("i1"); f.Severity = posttrade.SeverityNone; return f }(), "severity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink, cc := newBusSink(t)
			err := sink.Publish(context.Background(), tc.fail)
			if err == nil {
				t.Fatal("an incomplete fail was published")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the offending field %q", err, tc.want)
			}
			if len(cc.sent) != 0 {
				t.Errorf("nothing should reach the wire, got %d", len(cc.sent))
			}
		})
	}
}

func TestNewBusFailSinkNilProducer(t *testing.T) {
	if _, err := posttrade.NewBusFailSink(nil); err == nil {
		t.Error("nil producer should error")
	}
}
