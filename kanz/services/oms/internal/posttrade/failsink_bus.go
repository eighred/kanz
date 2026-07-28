package posttrade

import (
	"context"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// Bus-backed FailSink (PARITY-04g): the concrete emitter that maps a settlement
// Fail onto the settlement.v1.SettlementFail FACT and publishes it on the bus,
// so the AUTO-01 controller (severity → remediation) and IBOR-01e reconciliation
// consume it. It wraps a shared bus.Producer — the RISK-10 stance — so
// producer_sequence stays monotonic across emitted fails.
//
// # The payload proto is an injected seam (settlement.v1 is generated-not-committed)
//
// settlement.v1 is generated-not-committed (EVT-15a — it is NOT in the checked-in
// gen/go tree, only its .proto), so this package builds its enveloping + publish
// logic against a FailEncoder seam rather than importing settlementpb. The
// composition root supplies the concrete `func(Fail) (*settlementpb.SettlementFail,
// error)` once the SDK is generated; the emitter here is fully tested with a fake
// encoder. This is the PARITY-02d/e Decoder-seam stance applied to production.
type BusFailSink struct {
	producer *bus.Producer
	encode   FailEncoder
	now      func() time.Time
}

// FailEncoder maps a Fail to its wire payload (the settlement.v1.SettlementFail
// proto). Returning an error aborts that publish.
type FailEncoder func(f Fail) (proto.Message, error)

const (
	domainSettlement        = "settlement"
	eventTypeSettlementFail = "settlement.instruction.fail"
	schemaRefSettlementFail = "settlement.v1.SettlementFail:1"
	schemaVersionFail       = 1
)

// NewBusFailSink builds a bus-backed FailSink over producer, mapping each Fail to
// its FACT via encode. Errors when producer or encode is nil.
func NewBusFailSink(producer *bus.Producer, encode FailEncoder) (*BusFailSink, error) {
	return NewBusFailSinkWithClock(producer, encode, time.Now)
}

// NewBusFailSinkWithClock is NewBusFailSink with an injectable clock so the
// emitted event_time is deterministic in tests (the package convention).
func NewBusFailSinkWithClock(producer *bus.Producer, encode FailEncoder, now func() time.Time) (*BusFailSink, error) {
	if producer == nil {
		return nil, errors.New("posttrade: nil producer")
	}
	if encode == nil {
		return nil, errors.New("posttrade: nil fail encoder")
	}
	if now == nil {
		now = time.Now
	}
	return &BusFailSink{producer: producer, encode: encode, now: now}, nil
}

var _ FailSink = (*BusFailSink)(nil)

// Publish emits f as a settlement.v1.SettlementFail FACT, partitioned by the
// instruction id so a settlement's fail events stay ordered. IdempotencyKey is
// left empty — the producer stamps it to event_id for a FACT.
func (s *BusFailSink) Publish(ctx context.Context, f Fail) error {
	payload, err := s.encode(f)
	if err != nil {
		return err
	}
	if payload == nil {
		return errors.New("posttrade: fail encoder returned nil payload")
	}
	return s.producer.Publish(ctx, bus.Event{
		Subject:          eventTypeSettlementFail,
		EventType:        eventTypeSettlementFail,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    schemaVersionFail,
		Domain:           domainSettlement,
		EventTime:        s.now(),
		PartitionKey:     f.InstructionID,
		PayloadSchemaRef: schemaRefSettlementFail,
		Payload:          payload,
	})
}
