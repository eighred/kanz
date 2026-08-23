package posttrade

import (
	"context"
	"errors"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// Bus-backed FailSink (PARITY-04g): the concrete emitter that maps a settlement
// Fail onto the settlement.v1.SettlementFail FACT and publishes it on the bus,
// so the AUTO-01 controller (severity → remediation) and IBOR-01e reconciliation
// consume it. It wraps a shared bus.Producer — the RISK-10 stance — so
// producer_sequence stays monotonic across emitted fails.
//
// # The payload is encoded here, and used to be an injected seam
//
// This carried a FailEncoder the composition root was meant to supply, justified
// by "settlement.v1 is generated-not-committed (EVT-15a — it is NOT in the
// checked-in gen/go tree, only its .proto)". THE PREMISE WAS FALSE:
// kanz-schemas/gen is not checked in at all, so every generated package is
// generated-not-committed, and this package already imports four of them
// (order/v1, common/v1, envelope/v1, observation/v1 — settle.go takes an
// *orderpb.Fill). The seam was an indirection with one possible implementation,
// resting on a reason its own import block contradicted, and no composition root
// ever supplied it. See failsink_encode.go, which is now that implementation.
//
// Closing it does NOT arm the post-trade plane: nothing constructs a Settlement,
// so nothing detects a fail, so nothing publishes one. settlement_posture.go in
// the OMS composition root reports every stage as not-running and names the
// counterparty confirmation feed as what would arm them (#589).
type BusFailSink struct {
	producer *bus.Producer
	now      func() time.Time
}

const (
	domainSettlement        = "settlement"
	eventTypeSettlementFail = "settlement.instruction.fail"
	schemaRefSettlementFail = "settlement.v1.SettlementFail:1"
	schemaVersionFail       = 1
)

// NewBusFailSink builds a bus-backed FailSink over producer, encoding each Fail
// with EncodeFail. Errors when producer is nil.
func NewBusFailSink(producer *bus.Producer) (*BusFailSink, error) {
	return NewBusFailSinkWithClock(producer, time.Now)
}

// NewBusFailSinkWithClock is NewBusFailSink with an injectable clock so the
// emitted event_time is deterministic in tests (the package convention).
func NewBusFailSinkWithClock(producer *bus.Producer, now func() time.Time) (*BusFailSink, error) {
	if producer == nil {
		return nil, errors.New("posttrade: nil producer")
	}
	if now == nil {
		now = time.Now
	}
	return &BusFailSink{producer: producer, now: now}, nil
}

var _ FailSink = (*BusFailSink)(nil)

// Publish emits f as a settlement.v1.SettlementFail FACT, partitioned by the
// instruction id so a settlement's fail events stay ordered. IdempotencyKey is
// left empty — the producer stamps it to event_id for a FACT.
func (s *BusFailSink) Publish(ctx context.Context, f Fail) error {
	payload, err := EncodeFail(f)
	if err != nil {
		return err
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
