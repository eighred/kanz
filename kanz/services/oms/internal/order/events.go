package order

import (
	"context"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/outbox"
)

// Subjects follow the {domain}.{entity}.{event_type} taxonomy. Commands are
// consumed on the .submit/.amend/.cancel subjects; the OMS publishes the
// lifecycle FACTs and the universal command outcome on the rest.
const (
	Domain = "order"

	SubjectSubmit = "order.order.submit"
	SubjectAmend  = "order.order.amend"
	SubjectCancel = "order.order.cancel"

	EventTypeAccepted        = "order.order.accepted"
	EventTypeRejected        = "order.order.rejected"
	EventTypeRouted          = "order.order.routed"
	EventTypePartiallyFilled = "order.order.partially_filled"
	EventTypeFilled          = "order.order.filled"
	EventTypeCancelled       = "order.order.cancelled"
	EventTypeExpired         = "order.order.expired"
	EventTypeOutcome         = "order.order.outcome"
)

// payloadSchemaRef returns the EVT-16 registry ref for an order.v1 message. The
// schema-registry resolves it for the lake-sink/decoders; an unregistered ref
// lands envelope-only rather than failing (LAKE-01a), so emitting is safe ahead
// of registration.
func payloadSchemaRef(message string) string { return "order.v1." + message + ":1" }

// Bus is the publish surface the OMS needs — satisfied by *bus.Producer. Narrow
// interface so the command handler and executor are testable with a fake that
// records emitted events.
type Bus interface {
	Publish(ctx context.Context, e bus.Event) error
}

// Emitter publishes the order lifecycle FACTs and command outcomes. The
// underlying Producer auto-stamps envelope identity + lineage from ctx, so a
// FACT emitted while handling a command is causation-linked to that command.
type Emitter struct{ b Bus }

// NewEmitter wraps a Bus.
func NewEmitter(b Bus) *Emitter { return &Emitter{b: b} }

// publisher hands the outbox relay the SAME bus this emitter publishes through
// (#292). One bus, so a FACT that goes out through the relay and one that goes
// out directly cannot end up on different clients — and so a test that injects a
// fake bus into the emitter is injecting it into the relay too, rather than
// leaving the relay pointed at something the test never sees.
func (e *Emitter) publisher() outbox.Publisher { return e.b }

// event builds one FACT for order orderID at event-time t.
//
// IT IS SEPARATE FROM PUBLISHING, AND THAT SEPARATION IS THE #292 SEAM. A FACT
// now has two possible sinks: the broker, right now (every transition that has
// not yet been converted), and the outbox, inside the transaction that caused it
// (admission). Those are two DESTINATIONS for ONE event definition, not two
// definitions — which is why this function exists and why nothing below builds a
// bus.Event of its own. A second builder is how the enqueued FACT and the
// published FACT would come to differ in a field nobody compares.
func (e *Emitter) event(eventType, message, orderID string, t time.Time, payload proto.Message) bus.Event {
	return bus.Event{
		Subject:          eventType,
		EventType:        eventType,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           Domain,
		EventTime:        t,
		PartitionKey:     orderID,
		PayloadSchemaRef: payloadSchemaRef(message),
		Payload:          payload,
	}
}

// emit publishes one FACT for order orderID at event-time t.
func (e *Emitter) emit(ctx context.Context, eventType, message, orderID string, t time.Time, payload proto.Message) error {
	return e.b.Publish(ctx, e.event(eventType, message, orderID, t, payload))
}

// acceptedEvent is the ORDER_ACCEPTED FACT for an admitted order. One function
// so the enqueued form and the compensator's published form cannot drift.
func (e *Emitter) acceptedEvent(st *orderpb.OrderState) bus.Event {
	return e.event(EventTypeAccepted, "OrderAccepted", st.GetOrderId(), st.GetAsOf().AsTime(),
		&orderpb.OrderAccepted{OrderId: st.GetOrderId(), State: st})
}

// EmitAccepted publishes OrderAccepted with the admitted state.
//
// THE LIVE ADMISSION PATH NO LONGER CALLS THIS (#292) — it calls AcceptedFact
// and hands the record to store.Create. What still calls it is
// reannounceAccepted, #238's compensator, which repairs an order admitted BEFORE
// the outbox existed: those rows carry accepted_announced_at unset with no
// outbox record behind them, so the only thing that can announce them is a
// direct publish. It stays until that population cannot exist; see
// reannounceAccepted for the retirement condition.
func (e *Emitter) EmitAccepted(ctx context.Context, st *orderpb.OrderState) error {
	return e.b.Publish(ctx, e.acceptedEvent(st))
}

// AcceptedFact captures the ORDER_ACCEPTED FACT as an outbox record instead of
// publishing it, so admission can commit the order and its announcement in one
// transaction.
//
// It takes ctx because the record has to carry the lineage the synchronous
// publish would have inherited from it — correlation, causation, trace and the
// tenant — and the relay that eventually sends this has none of them. See
// outbox.From, which refuses a record with no tenant rather than enqueueing one
// that can never be published.
func (e *Emitter) AcceptedFact(ctx context.Context, st *orderpb.OrderState) (outbox.Record, error) {
	return outbox.From(ctx, e.acceptedEvent(st))
}

// EmitRejected publishes OrderRejected (no state changed; order terminal).
func (e *Emitter) EmitRejected(ctx context.Context, orderID, code, reason string, t time.Time) error {
	return e.emit(ctx, EventTypeRejected, "OrderRejected", orderID, t,
		&orderpb.OrderRejected{OrderId: orderID, Reason: reason, ErrorCode: code})
}

// EmitRouted publishes OrderRouted (order working at venue).
func (e *Emitter) EmitRouted(ctx context.Context, orderID, venue, venueOrderID string, t time.Time) error {
	return e.emit(ctx, EventTypeRouted, "OrderRouted", orderID, t,
		&orderpb.OrderRouted{OrderId: orderID, Venue: venue, VenueOrderId: venueOrderID})
}

// EmitFill publishes OrderPartiallyFilled or OrderFilled depending on whether
// the fill completed the order.
func (e *Emitter) EmitFill(ctx context.Context, fill *orderpb.Fill, st *orderpb.OrderState) error {
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_FILLED {
		return e.emit(ctx, EventTypeFilled, "OrderFilled", st.GetOrderId(), fill.GetExecutedAt().AsTime(),
			&orderpb.OrderFilled{OrderId: st.GetOrderId(), Fill: fill, State: st})
	}
	return e.emit(ctx, EventTypePartiallyFilled, "OrderPartiallyFilled", st.GetOrderId(), fill.GetExecutedAt().AsTime(),
		&orderpb.OrderPartiallyFilled{OrderId: st.GetOrderId(), Fill: fill, State: st})
}

// EmitCancelled publishes OrderCancelled.
func (e *Emitter) EmitCancelled(ctx context.Context, orderID string, cancelledQty *commonpb.Decimal, t time.Time) error {
	return e.emit(ctx, EventTypeCancelled, "OrderCancelled", orderID, t,
		&orderpb.OrderCancelled{OrderId: orderID, CancelledQuantity: cancelledQty})
}

// EmitExpired publishes OrderExpired.
func (e *Emitter) EmitExpired(ctx context.Context, orderID string, unfilledQty *commonpb.Decimal, t time.Time) error {
	return e.emit(ctx, EventTypeExpired, "OrderExpired", orderID, t,
		&orderpb.OrderExpired{OrderId: orderID, UnfilledQuantity: unfilledQty})
}

// EmitOutcome publishes the universal command outcome FACT every command must
// produce (command.v1, event-class-rules §2). resultRef optionally points at
// the FACT carrying the command's effect.
func (e *Emitter) EmitOutcome(ctx context.Context, orderID string, status commandpb.CommandOutcomeStatus, reason, errorCode, resultRef string, t time.Time) error {
	return e.emit(ctx, EventTypeOutcome, "CommandOutcome", orderID, t,
		&commandpb.CommandOutcome{
			Status:      status,
			Reason:      reason,
			ErrorCode:   errorCode,
			ResultRef:   resultRef,
			CompletedAt: timestamppb.New(t.UTC()),
		})
}
