package main

import (
	"fmt"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
)

// The order.order.* event_type wire values this monitor renders, copied
// verbatim from services/oms/internal/order/events.go:20-35.
//
// They are copied, not imported: that package is Go-internal to
// services/oms (github.com/eighred/kanz/services/oms/internal/order), and
// the Go compiler forbids any importer outside services/oms/... from using
// it — confirmed with `go vet ./cmd/kanz-monitor/...` against a scratch
// import, which failed with "use of internal package ... not allowed". This
// is not an import-weight tradeoff to reconsider later; it is enforced by
// the language. A rename of these constants in events.go will not propagate
// here automatically and must be mirrored by hand.
const (
	eventTypeAccepted        = "order.order.accepted"
	eventTypeRejected        = "order.order.rejected"
	eventTypeRouted          = "order.order.routed"
	eventTypePartiallyFilled = "order.order.partially_filled"
	eventTypeFilled          = "order.order.filled"
	eventTypeCancelled       = "order.order.cancelled"
	eventTypeExpired         = "order.order.expired"
	eventTypeOutcome         = "order.order.outcome"
)

// decodeLifecycle maps an order.order.* FACT envelope+payload to a display
// row. It returns ok=false for an event_type this monitor does not render
// (order.order.accepted precedes routed and carries nothing new for the
// operator feed; order.order.outcome mirrors accept/reject/fill; anything
// else is simply unknown to this build) — the caller drops it silently
// rather than showing "unknown event" noise that would teach the operator to
// ignore the feed.
func decodeLifecycle(env *envelopepb.Envelope, payload []byte) (lifecycleEvent, bool) {
	row := lifecycleEvent{Type: env.GetEventType()}
	if t := env.GetEventTime(); t != nil {
		row.At = t.AsTime()
	}

	switch env.GetEventType() {
	case eventTypeRejected:
		var ev orderpb.OrderRejected
		if err := proto.Unmarshal(payload, &ev); err != nil {
			return lifecycleEvent{}, false
		}
		row.OrderID = ev.GetOrderId()
		row.Detail = fmt.Sprintf("%s: %s", ev.GetErrorCode(), ev.GetReason())
		return row, true

	case eventTypeRouted:
		var ev orderpb.OrderRouted
		if err := proto.Unmarshal(payload, &ev); err != nil {
			return lifecycleEvent{}, false
		}
		row.OrderID = ev.GetOrderId()
		row.Detail = fmt.Sprintf("routed to %s (venue order %s)", ev.GetVenue(), ev.GetVenueOrderId())
		return row, true

	case eventTypePartiallyFilled:
		var ev orderpb.OrderPartiallyFilled
		if err := proto.Unmarshal(payload, &ev); err != nil {
			return lifecycleEvent{}, false
		}
		row.OrderID = ev.GetOrderId()
		row.Detail = fillDetail(ev.GetFill(), ev.GetState())
		return row, true

	case eventTypeFilled:
		var ev orderpb.OrderFilled
		if err := proto.Unmarshal(payload, &ev); err != nil {
			return lifecycleEvent{}, false
		}
		row.OrderID = ev.GetOrderId()
		row.Detail = fillDetail(ev.GetFill(), ev.GetState())
		return row, true

	case eventTypeCancelled:
		var ev orderpb.OrderCancelled
		if err := proto.Unmarshal(payload, &ev); err != nil {
			return lifecycleEvent{}, false
		}
		row.OrderID = ev.GetOrderId()
		row.Detail = fmt.Sprintf("cancelled, %s unfilled", dec.Str(dec.FromProto(ev.GetCancelledQuantity())))
		return row, true

	case eventTypeExpired:
		var ev orderpb.OrderExpired
		if err := proto.Unmarshal(payload, &ev); err != nil {
			return lifecycleEvent{}, false
		}
		row.OrderID = ev.GetOrderId()
		row.Detail = fmt.Sprintf("expired, %s unfilled", dec.Str(dec.FromProto(ev.GetUnfilledQuantity())))
		return row, true

	case eventTypeAccepted, eventTypeOutcome:
		return lifecycleEvent{}, false

	default:
		return lifecycleEvent{}, false
	}
}

// fillDetail renders a fill's venue-facing detail: the exact executed
// quantity and price off the wire (never a rounded float — this is a
// capital-path display), plus the order's resulting cumulative fill state.
func fillDetail(f *orderpb.Fill, s *orderpb.OrderState) string {
	return fmt.Sprintf("%s @ %s (venue %s), filled %s/%s",
		dec.Str(dec.FromProto(f.GetQuantity())),
		dec.Str(dec.FromProto(f.GetPrice())),
		f.GetVenue(),
		dec.Str(dec.FromProto(s.GetFilledQuantity())),
		dec.Str(dec.FromProto(s.GetOrderedQuantity())),
	)
}

// decodePosition maps a domain.v1.PositionState FACT to a book row. The
// subscription (Task 3) scopes the subject to the position streams, so the
// payload is unambiguously a PositionState; ok=false only on an unparseable
// payload.
func decodePosition(env *envelopepb.Envelope, payload []byte) (position, bool) {
	// env.GetEventType() is intentionally not branched on: Task 3's
	// subscription scopes this call to the position stream, where a
	// PositionState is the only payload shape.
	var ev domainpb.PositionState
	if err := proto.Unmarshal(payload, &ev); err != nil {
		return position{}, false
	}
	return position{
		Portfolio:  ev.GetPortfolioId(),
		Instrument: ev.GetInstrumentId(),
		Quantity:   dec.Str(dec.FromProto(ev.GetQuantity())),
		AvgPrice:   dec.Str(dec.FromProto(ev.GetAveragePrice())),
	}, true
}
