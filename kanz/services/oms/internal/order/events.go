package order

import (
	"context"
	"fmt"
	"github.com/eighred/kanz/internal/fillfact"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dualcontrol"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/pkg/bus"
)

// Subjects follow the {domain}.{entity}.{event_type} taxonomy. Commands are
// consumed on the .submit/.amend/.cancel/.approve subjects; the OMS publishes
// the lifecycle FACTs and the universal command outcome on the rest.
const (
	Domain = "order"

	SubjectSubmit = "order.order.submit"
	SubjectAmend  = "order.order.amend"
	SubjectCancel = "order.order.cancel"
	// SubjectApprove carries the SECOND SIGNATURE on a held order (#539). It is
	// a command like the three above and not a query, because approving CAUSES
	// AN ADMISSION — it re-enters the same path a submission takes, with the
	// compliance gate, the venue checks and the outbox all still in front of it.
	SubjectApprove = "order.order.approve"

	EventTypeAccepted = "order.order.accepted"
	EventTypeRejected = "order.order.rejected"
	// EventTypePendingApproval announces an order HELD for a second signature
	// (#410). It is NOT a lifecycle status: a held order is not in the order
	// book at all, so this is the only thing the estate ever hears about it
	// until it is approved (an ORDER_ACCEPTED under the same order_id) or it
	// expires.
	EventTypePendingApproval = "order.order.pending_approval"
	// EventTypeApproved announces that a held order received its SECOND
	// SIGNATURE (#410, clause (d)). It is NOT a lifecycle status either: the
	// order's own outcome still follows under the same order_id, because an
	// approved order replays every gate and can still be refused.
	//
	// IT IS THE ONLY THING ON THE BUS THAT NAMES THE APPROVER. ORDER_ACCEPTED
	// carries an OrderState, which records the order and not who released it,
	// and the ACCEPTED CommandOutcome names neither party — so without this the
	// answer to "who countersigned this" lived only in the OMS's own
	// order_proposals table, which services/audit cannot read.
	EventTypeApproved        = "order.order.approved"
	EventTypeRouted          = "order.order.routed"
	EventTypePartiallyFilled = fillfact.SubjectPartiallyFilled
	EventTypeFilled          = fillfact.SubjectFilled
	EventTypeCancelled       = "order.order.cancelled"
	EventTypeExpired         = "order.order.expired"
	EventTypeOutcome         = "order.order.outcome"
)

// schemaVersion is the EVT-16 version every FACT this emitter builds carries.
// One constant so the envelope field and the registry ref cannot disagree —
// they were two independent literals, and a ref that disagrees with the
// envelope is the same class of defect payloadSchemaRef documents below.
const schemaVersion = 1

// payloadSchemaRef returns the EVT-16 registry ref for a FACT's payload,
// DERIVED FROM THE MESSAGE ITSELF rather than from a name passed alongside it.
// The schema-registry resolves it for the lake-sink/decoders; an unregistered
// ref lands envelope-only rather than failing (LAKE-01a), so emitting is safe
// ahead of registration.
//
// IT USED TO TAKE A STRING AND PREFIX IT WITH "order.v1.", AND THAT WAS A LIVE
// DEFECT. Every FACT here is an order.v1 message except one: the universal
// CommandOutcome, which is command.v1. So every command outcome the OMS has ever
// published carried payload_schema_ref "order.v1.CommandOutcome:1" — a type that
// does not exist in any descriptor. Nothing failed, because LAKE-01a treats an
// unresolvable ref as envelope-only: the outcomes landed in the lake with their
// payloads never decoded, which is indistinguishable from a schema not yet
// registered.
//
// It surfaced only when the amend outcome moved onto the outbox (#292), because
// outbox.Record.Event() must resolve the ref to rebuild the payload and refuses
// rather than guessing:
//
//	outbox: payload_schema_ref names a message this binary does not know:
//	"order.v1.CommandOutcome"
//
// Deriving it from the payload cannot disagree with the payload. It reproduces
// every previously-correct ref byte-identically — the order.v1 messages' full
// names are exactly "order.v1.X" — so this changes one ref and no others.
func payloadSchemaRef(payload proto.Message, version int32) string {
	return fmt.Sprintf("%s:%d", payload.ProtoReflect().Descriptor().FullName(), version)
}

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
// (admission, and the fill folds). Those are two DESTINATIONS for ONE event definition, not two
// definitions — which is why this function exists and why nothing below builds a
// bus.Event of its own. A second builder is how the enqueued FACT and the
// published FACT would come to differ in a field nobody compares.
func (e *Emitter) event(eventType, orderID string, t time.Time, payload proto.Message) bus.Event {
	return bus.Event{
		Subject:          eventType,
		EventType:        eventType,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    schemaVersion,
		Domain:           Domain,
		EventTime:        t,
		PartitionKey:     orderID,
		PayloadSchemaRef: payloadSchemaRef(payload, schemaVersion),
		Payload:          payload,
	}
}

// The `emit` shortcut that used to live here is gone (#435). EmitExpired was its
// last caller, and giving expiry a named *Event builder — so its published and
// its ENQUEUED forms cannot drift, the rule every other FACT here already
// follows — left nothing behind it. A helper kept for symmetry that no longer
// has a caller is the shape a second way of publishing grows back from.

// acceptedEvent is the ORDER_ACCEPTED FACT for an admitted order. One function
// so the enqueued form and the compensator's published form cannot drift.
func (e *Emitter) acceptedEvent(st *orderpb.OrderState) bus.Event {
	return e.event(EventTypeAccepted, st.GetOrderId(), st.GetAsOf().AsTime(),
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

// pendingApprovalEvent is the ORDER_PENDING_APPROVAL FACT for an order HELD for
// a second signature (#410). One builder, so the enqueued form cannot drift from
// any later published one.
func (e *Emitter) pendingApprovalEvent(p OrderProposal) bus.Event {
	return e.event(EventTypePendingApproval, p.ID, p.CreatedAt,
		&orderpb.OrderPendingApproval{
			OrderId:    p.ID,
			Command:    p.Command,
			Proposer:   p.Proposer,
			Act:        string(p.Act),
			Digest:     p.Digest,
			ProposedAt: timestamppb.New(p.CreatedAt),
			ExpiresAt:  timestamppb.New(p.ExpiresAt),
		})
}

// PendingApprovalFact captures ORDER_PENDING_APPROVAL as an outbox record, so
// the proposal row and the announcement that the order is held commit in ONE
// transaction (#292's guarantee applied to #410's hold).
//
// THERE IS NO EmitPendingApproval, DELIBERATELY. This FACT has exactly one
// producer — the hold in handleSubmit — and it always has a proposal row to
// commit alongside. A direct-publish sibling would be a second way to announce a
// held order, available to a caller with no row to commit, and the only thing it
// could produce is an announcement of a hold that never happened. Every builder
// in this file that grew such a sibling grew it for a compensator repairing rows
// written before the outbox existed; there is no such population here, because
// this FACT is younger than the outbox.
//
// It takes ctx for the same reason AcceptedFact does: the record must carry the
// lineage and the TENANT the synchronous publish would have inherited, and the
// relay has none of them. outbox.From refuses a record with no tenant rather
// than enqueueing one that can never be published — and because the enqueue is
// INSIDE the proposal transaction, that refusal ROLLS THE HOLD BACK too, which
// is the correct direction: an order whose hold cannot be announced must not be
// held silently. The tenant is present on this path because a SubmitOrder
// arrives as a bus DELIVERY, exactly as it is for AcceptedFact three lines up;
// it is the HTTP-borne case that has none, which is what cost #498 an outage.
func (e *Emitter) PendingApprovalFact(ctx context.Context, p OrderProposal) (outbox.Record, error) {
	return outbox.From(ctx, e.pendingApprovalEvent(p))
}

// approvedEvent is the ORDER_APPROVED FACT for a held order that a second
// subject signed (#410, clause (d)).
//
// IT TAKES THE Approval, NOT TWO STRINGS, and that is the whole reason both
// names on this FACT are worth reading. dualcontrol.Approval is evidence
// obtainable only by passing the self-approval, digest and expiry checks, and
// its fields are unexported so no package outside dualcontrol can forge one by
// composite literal. A builder taking `proposer, approver string` could be
// handed two names by one person and nothing in its signature would say
// otherwise — which is exactly the shape internal/compliance's publisher was
// before #511, and it is why that one takes an Approval too. One rule, one
// place, three acts.
//
// Every field here is read off the evidence. There is no argument left for a
// lone actor and none for a name the check never saw.
func (e *Emitter) approvedEvent(a dualcontrol.Approval) bus.Event {
	return e.event(EventTypeApproved, a.Subject(), a.ApprovedAt(),
		&orderpb.OrderApproved{
			OrderId:    a.Subject(),
			Proposer:   a.Proposer(),
			Approver:   a.Approver(),
			Act:        string(a.Act()),
			Digest:     a.Digest(),
			ApprovedAt: timestamppb.New(a.ApprovedAt()),
		})
}

// ApprovedFact captures ORDER_APPROVED as an outbox record, so the second
// signature and the announcement of it commit in ONE transaction — the shape
// #498 gave act one and #511 gave act two, arriving here on the act where the
// capital actually moves.
//
// THERE IS NO EmitApproved, DELIBERATELY, and for the same reason there is no
// EmitPendingApproval. This FACT has exactly one producer — the claim in
// handleApprove — and it always has a proposal row to commit alongside. A
// direct-publish sibling would be a second way to announce an approval,
// available to a caller with no claim to commit, and the only thing it could
// produce is an announcement of a signature nobody gave. Every builder in this
// file that grew such a sibling grew it for a compensator repairing rows written
// before the outbox existed; there is no such population here.
//
// It takes ctx for the tenant and the lineage, exactly as its siblings do.
// outbox.From refuses a record with no tenant, and because the enqueue is INSIDE
// the claim transaction that refusal ROLLS THE CLAIM BACK — the correct
// direction: a second signature that cannot be announced must not be silently
// recorded either. The tenant is present on this path because an ApproveOrder
// arrives as a bus DELIVERY (SubjectApprove), not over HTTP; it is the
// HTTP-borne case that has none, which is what cost #498 an outage.
func (e *Emitter) ApprovedFact(ctx context.Context, a dualcontrol.Approval) (outbox.Record, error) {
	return outbox.From(ctx, e.approvedEvent(a))
}

// EmitRejected publishes OrderRejected (no state changed; order terminal).
func (e *Emitter) EmitRejected(ctx context.Context, orderID, code, reason string, t time.Time) error {
	return e.b.Publish(ctx, e.rejectedEvent(orderID, code, reason, t))
}

// rejectedEvent is the ORDER_REJECTED FACT. One builder, two sinks — see event().
func (e *Emitter) rejectedEvent(orderID, code, reason string, t time.Time) bus.Event {
	return e.event(EventTypeRejected, orderID, t,
		&orderpb.OrderRejected{OrderId: orderID, Reason: reason, ErrorCode: code})
}

// RejectedFact captures ORDER_REJECTED as an outbox record so a rejection and
// its announcement commit together (#292).
//
// WHY THIS ONE MATTERED MOST of the pairs that had a compensator. The marker
// outcome_announced_at drives completeTerminalOutcome, which rebuilds the
// CommandOutcome from stored state — but it CANNOT rebuild this FACT, and says
// so in its own comment. So a crash between the Save and the publish left the
// order durably REJECTED, the caller eventually answered by the compensator, and
// the ORDER_REJECTED FACT gone from the estate with nothing able to notice: the
// ledger and every downstream projection would never learn the order died.
func (e *Emitter) RejectedFact(ctx context.Context, orderID, code, reason string, t time.Time) (outbox.Record, error) {
	return outbox.From(ctx, e.rejectedEvent(orderID, code, reason, t))
}

// routedEvent is the ORDER_ROUTED FACT. One builder, two sinks — see event().
func (e *Emitter) routedEvent(orderID, venue, venueOrderID string, t time.Time) bus.Event {
	return e.event(EventTypeRouted, orderID, t,
		&orderpb.OrderRouted{OrderId: orderID, Venue: venue, VenueOrderId: venueOrderID})
}

// EmitRouted publishes OrderRouted (order working at venue).
//
// THE LIVE ROUTING PATH NO LONGER CALLS THIS (#292) — work() calls RoutedFact
// and hands the record to store.Save alongside the ROUTED state. It is kept
// exported because the FACT is rebuildable from stored state (unlike a fill), so
// a compensator for it remains possible; nothing calls it today.
func (e *Emitter) EmitRouted(ctx context.Context, orderID, venue, venueOrderID string, t time.Time) error {
	return e.b.Publish(ctx, e.routedEvent(orderID, venue, venueOrderID, t))
}

// RoutedFact captures ORDER_ROUTED as an outbox record so the transition to
// ROUTED and its announcement commit together (#292).
//
// WHY THIS PAIR WAS WORTH CONVERTING even though a compensator could rebuild it:
// there was no compensator. ROUTED is the one transition with no
// *_announced_at marker at all, so a crash between the Save and the publish left
// an order stored as working at a venue with nothing downstream told — and
// nothing to notice. The three markers cover admission, cancellation and
// terminal outcomes; this fell between them.
func (e *Emitter) RoutedFact(ctx context.Context, orderID, venue, venueOrderID string, t time.Time) (outbox.Record, error) {
	return outbox.From(ctx, e.routedEvent(orderID, venue, venueOrderID, t))
}

// fillEvent is the ORDER_FILLED or ORDER_PARTIALLY_FILLED FACT for one fill,
// chosen by whether the fill completed the order.
//
// The FACT carries the individual Fill — fill_id, price, venue_execution_id,
// executed_at — and NOTHING ELSE IN THE PLATFORM DOES. The stored OrderState
// keeps only the cumulative aggregate, which is why completeTerminalOutcome
// cannot rebuild this event and says so, and why it is the one FACT that had to
// stop being a second independent write.
func (e *Emitter) fillEvent(fill *orderpb.Fill, st *orderpb.OrderState) bus.Event {
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_FILLED {
		return e.event(EventTypeFilled, st.GetOrderId(), fill.GetExecutedAt().AsTime(),
			&orderpb.OrderFilled{OrderId: st.GetOrderId(), Fill: fill, State: st})
	}
	return e.event(EventTypePartiallyFilled, st.GetOrderId(), fill.GetExecutedAt().AsTime(),
		&orderpb.OrderPartiallyFilled{OrderId: st.GetOrderId(), Fill: fill, State: st})
}

// FillFact captures the fill FACT as an outbox record instead of publishing it,
// so the fold can commit the new order state and its announcement in one
// transaction (#292).
//
// THERE IS NO EmitFill ANY MORE, AND THAT IS DELIBERATE. Every other lifecycle
// FACT still has a direct emitter because a compensator can rebuild it from
// stored state; this one cannot be rebuilt by anything, so a way to publish it
// outside the transaction is a way to lose it. Leaving the direct call as "the
// simple option" is how the next fold would quietly stop being atomic — the
// arch guard in test/arch/oms_outbox_test.go fails if it comes back.
//
// It takes ctx for the same reason AcceptedFact does: the record has to carry
// the lineage the synchronous publish would have inherited — correlation,
// causation, trace and the tenant — and the relay that eventually sends it has
// none of them. outbox.From refuses a record with no tenant, and because this is
// called BEFORE the Save, that refusal stops the fold instead of committing a
// fill whose FACT could never be published.
func (e *Emitter) FillFact(ctx context.Context, fill *orderpb.Fill, st *orderpb.OrderState) (outbox.Record, error) {
	return outbox.From(ctx, e.fillEvent(fill, st))
}

// cancelledEvent is the ORDER_CANCELLED FACT. One builder, two sinks — see
// event(). It matters more here than for the other FACTs: the live path enqueues
// this and the compensator publishes it, and a compensator whose FACT differed
// in a field from the one it is standing in for would repair the estate into a
// state the live path never produces.
func (e *Emitter) cancelledEvent(orderID string, cancelledQty *commonpb.Decimal, t time.Time) bus.Event {
	return e.event(EventTypeCancelled, orderID, t,
		&orderpb.OrderCancelled{OrderId: orderID, CancelledQuantity: cancelledQty})
}

// EmitCancelled publishes OrderCancelled.
//
// THE LIVE CANCEL PATH NO LONGER CALLS THIS (#292) — handleCancel calls
// CancelledFact and hands the record to the same store.Save that writes the
// CANCELLED state. What still calls it is completeCancelAnnouncement, #238's
// compensator, which finishes an announcement for a row that has NO outbox
// record behind it: a cancel saved before this change, or before the outbox
// existed. Those can only be announced by a direct publish, which is why this
// stays; see completeCancelAnnouncement for the retirement condition.
func (e *Emitter) EmitCancelled(ctx context.Context, orderID string, cancelledQty *commonpb.Decimal, t time.Time) error {
	return e.b.Publish(ctx, e.cancelledEvent(orderID, cancelledQty, t))
}

// CancelledFact captures ORDER_CANCELLED as an outbox record so a cancellation
// and its announcement commit together (#292).
//
// WHAT THIS CLOSES. The cancel pair was three writes — Save(CANCELLED), publish
// the FACT, publish the outcome, then Save(cancel_announced_at) — and a failure
// at any of the middle steps left an order the ledger calls cancelled with the
// world still told it is live. cancel_announced_at and handleCancel's
// already-CANCELLED branch made that recoverable rather than lost, at the cost
// of a documented duplicate-FACT window on every recovery. Riding the write
// removes the window instead of compensating for it.
func (e *Emitter) CancelledFact(ctx context.Context, orderID string, cancelledQty *commonpb.Decimal, t time.Time) (outbox.Record, error) {
	return outbox.From(ctx, e.cancelledEvent(orderID, cancelledQty, t))
}

func (e *Emitter) expiredEvent(orderID string, unfilledQty *commonpb.Decimal, t time.Time) bus.Event {
	return e.event(EventTypeExpired, orderID, t,
		&orderpb.OrderExpired{OrderId: orderID, UnfilledQuantity: unfilledQty})
}

// EmitExpired publishes OrderExpired.
func (e *Emitter) EmitExpired(ctx context.Context, orderID string, unfilledQty *commonpb.Decimal, t time.Time) error {
	return e.b.Publish(ctx, e.expiredEvent(orderID, unfilledQty, t))
}

// ExpiredFact captures ORDER_EXPIRED as an outbox record so an expiry and its
// announcement commit together (#292), the same shape CancelledFact has.
//
// IT IS WHAT RETIRES A WORKED-OUT PARENT ORDER (#435). A parent has no fills of
// its own — its children reported every one — so ORDER_FILLED would be a second
// record of an execution that already happened, and any consumer summing filled
// quantities would double the fund's traded volume. ORDER_EXPIRED is the one
// terminal FACT carrying a QUANTITY rather than a Fill, which is exactly what a
// container that never traded has to announce: its window elapsed, and this much
// was never filled.
func (e *Emitter) ExpiredFact(ctx context.Context, orderID string, unfilledQty *commonpb.Decimal, t time.Time) (outbox.Record, error) {
	return outbox.From(ctx, e.expiredEvent(orderID, unfilledQty, t))
}

// outcomeEvent is the universal command outcome FACT. One builder, two sinks.
func (e *Emitter) outcomeEvent(orderID string, status commandpb.CommandOutcomeStatus, reason, errorCode, resultRef string, t time.Time) bus.Event {
	return e.event(EventTypeOutcome, orderID, t,
		&commandpb.CommandOutcome{
			Status:      status,
			Reason:      reason,
			ErrorCode:   errorCode,
			ResultRef:   resultRef,
			CompletedAt: timestamppb.New(t.UTC()),
		})
}

// EmitOutcome publishes the universal command outcome FACT every command must
// produce (command.v1, event-class-rules §2). resultRef optionally points at
// the FACT carrying the command's effect.
//
// STILL THE RIGHT CALL IN THREE SHAPES, and they share one property (#292):
// there is no state change for the outcome to commit alongside.
//
//  1. A command REFUSED before anything was written — outcomeReject, and refuse.
//  2. The #238 compensators re-announcing from a marker for rows with no outbox
//     record behind them — completeCancelAnnouncement, completeTerminalOutcome.
//  3. handleSubmit's trailing ACCEPTED outcome for an order that did NOT fill.
//     The order is resting or working; nothing terminal happened, so there is no
//     write to ride and no marker it would be truthful to stamp. Its FILLED
//     sibling DOES have a write — the outcome_announced_at stamp — and takes
//     OutcomeFact instead.
//
// An outcome with no accompanying state change has no transaction to ride in —
// enqueuing it would need a transaction opened solely to carry it, which is a
// worse trade than publishing it directly and is not what an outbox is for.
func (e *Emitter) EmitOutcome(ctx context.Context, orderID string, status commandpb.CommandOutcomeStatus, reason, errorCode, resultRef string, t time.Time) error {
	return e.b.Publish(ctx, e.outcomeEvent(orderID, status, reason, errorCode, resultRef, t))
}

// OutcomeFact captures the command outcome as an outbox record, for the
// transitions that DO write state — so the state change and the outcome the
// caller is waiting on commit together (#292).
func (e *Emitter) OutcomeFact(ctx context.Context, orderID string, status commandpb.CommandOutcomeStatus, reason, errorCode, resultRef string, t time.Time) (outbox.Record, error) {
	return outbox.From(ctx, e.outcomeEvent(orderID, status, reason, errorCode, resultRef, t))
}
