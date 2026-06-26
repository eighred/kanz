package order

import (
	"context"
	"time"

	commandpb "github.com/kanz-eng/kanz-schemas-go/command/v1"
	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/pkg/bus"
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

// emit publishes one FACT for order orderID at event-time t.
func (e *Emitter) emit(ctx context.Context, eventType, message, orderID string, t time.Time, payload proto.Message) error {
	return e.b.Publish(ctx, bus.Event{
		Subject:          eventType,
		EventType:        eventType,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           Domain,
		EventTime:        t,
		PartitionKey:     orderID,
		PayloadSchemaRef: payloadSchemaRef(message),
		Payload:          payload,
	})
}

// EmitAccepted publishes OrderAccepted with the admitted state.
func (e *Emitter) EmitAccepted(ctx context.Context, st *orderpb.OrderState) error {
	return e.emit(ctx, EventTypeAccepted, "OrderAccepted", st.GetOrderId(), st.GetAsOf().AsTime(),
		&orderpb.OrderAccepted{OrderId: st.GetOrderId(), State: st})
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
