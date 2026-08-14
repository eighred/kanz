package order

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"google.golang.org/protobuf/proto"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/schedule"
)

// ScheduleIssuer is the issuer stamped on a child order's command metadata.
//
// DELIBERATELY NOT A "user:" PRINCIPAL. delegatedAndEntitled only narrows
// entitlement for issuers carrying that prefix — a platform-issued command is
// already inside the boundary a user principal is being checked against. Naming
// the driver here is what makes a child traceable to the thing that created it
// in the audit log, which "" would not.
const ScheduleIssuer = "service:oms-schedule"

// DriveSchedules advances every parent order this OMS is working, once.
//
// # It is a TICK, and it holds nothing between ticks
//
// Everything it needs it re-derives: the parents from their status, the schedule
// from each parent's durable fields, and which children exist from the store. So
// a pod that has just booted does exactly what the pod it replaced would have
// done on the same tick, and running it twice concurrently creates no child
// twice — the child's id is derived, so the store's primary key refuses the
// second attempt rather than admitting a duplicate order.
//
// That is why this needs no cursor, no lease, and no leader election. It is also
// why a tick that fails partway is harmless: the next one re-derives whatever it
// did not get to.
//
// # What bounds how late a slice can be
//
// A child is sent on the first tick at or after it becomes due, so the interval
// this is called on is the WORST-CASE LATENESS of every slice. An operator
// working an order in one-minute slices on a five-minute tick is not getting a
// one-minute TWAP, and no error will tell them so — which is why the driver
// reports its own interval against the tightest schedule it is working (see
// TightestSliceInterval) rather than leaving that to be discovered from fills.
//
// It returns the number of children it created.
func (s *Service) DriveSchedules(ctx context.Context) (int, error) {
	// A CHILD'S ADMISSION PUBLISHES FACTs, SO IT NEEDS A TENANT BEFORE IT NEEDS
	// ANYTHING ELSE — the same requirement, for the same reason, as the sweep.
	// This runs on a timer, before and outside any delivery, so there is no
	// inbound envelope to inherit one from. Without it the first child would fail
	// envelope validation deep inside admission, and the operator would see a
	// parent that never progresses rather than this sentence.
	tenant := bus.TenantIDFromContext(ctx)
	if tenant == "" {
		return 0, errors.New("oms: DriveSchedules requires a tenant on ctx (bus.WithTenantID) — " +
			"it admits child orders, which publish FACTs, and it runs on a timer rather than on a " +
			"delivery, so unlike a handler it has no inbound envelope to inherit a tenant from")
	}

	parents, err := s.store.ListByStatus(ctx, orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED)
	if err != nil {
		return 0, fmt.Errorf("oms: could not list working parent orders: %w", err)
	}

	created := 0
	// COLLECTED, NOT RETURNED ON SIGHT — the same stance the PERIODIC sweep takes
	// and for the same reason. ListByStatus returns ORDER BY order_id, so one
	// parent that fails every tick (an unworkable schedule, a store row that will
	// not decode) would abort at the same point every time and permanently shadow
	// every parent sorting after it. Nothing would say so: the pod stays up, the
	// driver "runs", and those orders are never worked again.
	var failures []error
	now := s.now().UTC()

	for _, parentSt := range parents {
		if parentSt.GetQuarantine() != nil {
			continue // frozen; a human owns it
		}
		n, err := s.driveOne(ctx, parentSt, tenant, now)
		created += n
		if err != nil {
			s.logger.Error("oms: could not advance a working parent order — continuing with the "+
				"rest of the book so one stuck parent cannot shadow every parent behind it",
				"order_id", parentSt.GetOrderId(), "err", err)
			failures = append(failures, fmt.Errorf("parent %s: %w", parentSt.GetOrderId(), err))
		}
	}
	return created, errors.Join(failures...)
}

// driveOne emits the children of one parent that are due and do not yet exist.
func (s *Service) driveOne(ctx context.Context, parentSt *orderpb.OrderState, tenant string, now time.Time) (int, error) {
	parent, err := parentOf(parentSt)
	if err != nil {
		// A PARENT RESTING WITHOUT A DERIVABLE SCHEDULE IS LOUD, NOT SKIPPED.
		// Its status says it is being worked and nothing can work it; silence here
		// is exactly the state where "nothing configured" and "checked, and fine"
		// look the same.
		return 0, err
	}

	children, err := s.store.ListByParent(ctx, parent.OrderID)
	if err != nil {
		return 0, fmt.Errorf("could not read children: %w", err)
	}
	held := make(map[string]bool, len(children))
	for _, c := range children {
		held[c.GetOrderId()] = true
	}

	due, err := schedule.Due(parent, func(id string) bool { return held[id] }, now)
	if err != nil {
		return 0, err
	}

	created := 0
	for _, child := range due {
		// ORDERED BY SLICE INDEX, and this loop stops on the first failure rather
		// than skipping past it. What that leaves is a PREFIX of the schedule
		// sent, which the next tick resumes from — the alternative, carrying on,
		// would send slice 5 while slice 4 was missing, and a venue that saw the
		// quantity arrive out of order has been told a different story about how
		// this order was worked.
		if err := s.emitChild(ctx, parentSt, child, tenant, now); err != nil {
			return created, fmt.Errorf("slice %d: %w", child.Index, err)
		}
		created++
	}
	return created, nil
}

// emitChild admits one child order.
//
// IT GOES THROUGH ADMISSION, NOT AROUND IT. handleSubmit is the one place an
// order comes into existence on this platform: it validates, it takes the
// per-order claim, it writes the row and its ORDER_ACCEPTED FACT in one
// transaction, and it routes. A second creation path for children would be a
// second answer to "what is an admitted order", and the first thing to diverge
// would be whichever of the two somebody forgot to fix.
//
// The synthesized envelope is not a pretence that this came off the bus — it is
// the tenant, which admission needs to publish anything at all, in the shape
// admission already reads it from.
//
// NO DURABILITY IS NEEDED FOR THE COMMAND ITSELF, which is why this is a direct
// call rather than a publish. A command lost to a crash is re-derived by the next
// tick, because the driver asks what EXISTS rather than what it has sent. That
// property is the whole design, and it is what makes the cheap option also the
// correct one.
func (s *Service) emitChild(ctx context.Context, parentSt *orderpb.OrderState, child schedule.Child, tenant string, now time.Time) error {
	qty, ok := dec.ToProtoScaled(child.Quantity)
	if !ok {
		// The slice cannot be represented on the wire at all. Refusing here beats
		// sending a rounded quantity: a child admitted for less than its slice
		// leaves the parent unable to complete by the difference, forever, and
		// nothing downstream would ever say which slice was short.
		return fmt.Errorf("slice quantity %s cannot be represented as a decimal — the parent "+
			"cannot be worked without losing part of it", child.Quantity.FloatString(20))
	}

	cmd := &orderpb.SubmitOrder{
		Metadata: &commandpb.CommandMetadata{
			// target_id MUST equal order_id — the command-class contract every
			// order command keeps.
			TargetId: child.OrderID,
			Issuer:   ScheduleIssuer,
			Reason: fmt.Sprintf("slice %d of %d for parent order %s",
				child.Index+1, parentSt.GetExecutionSchedule().GetSliceCount(), child.ParentID),
			// NO principal_portfolios. That list NARROWS a delegated user
			// principal's authority, and this command has no user behind it — the
			// authority it acts on is the parent's, which was checked when the
			// parent was admitted. An empty list here is not an omission: with a
			// non-"user:" issuer, delegatedAndEntitled never consults it.
		},
		OrderId:       child.OrderID,
		ParentOrderId: child.ParentID,
		PortfolioId:   parentSt.GetPortfolioId(),
		InstrumentId:  parentSt.GetInstrumentId(),
		Side:          parentSt.GetSide(),
		Quantity:      qty,
		OrderType:     parentSt.GetOrderType(),
		LimitPrice:    parentSt.GetLimitPrice(),
		StopPrice:     parentSt.GetStopPrice(),
		TimeInForce:   parentSt.GetTimeInForce(),
		ExpireAt:      parentSt.GetExpireAt(),
		// THE PARENT'S VENUE, SO EVERY SLICE OF ONE DECISION TRADES WHERE THAT
		// DECISION SAID. Empty when the parent named none, which leaves each child
		// to the router's own choice — and that is deliberate rather than
		// overlooked: an unrouted parent is one whose venue the platform picks, and
		// picking it per slice is what lets the cost ranker (#437) move later
		// slices to whichever venue the earlier ones proved cheaper.
		Venue: parentSt.GetVenue(),
		// NO execution_schedule. A child is not itself worked as a schedule, and
		// validateSchedule refuses a command carrying both.
	}
	payload, err := proto.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal child order: %w", err)
	}
	env := &envelopepb.Envelope{
		EventId:      child.OrderID,
		EventType:    "order.submit",
		TenantId:     tenant,
		PartitionKey: child.OrderID,
	}
	if err := s.handleSubmit(ctx, env, payload); err != nil {
		return err
	}
	s.logger.Info("oms: sent a scheduled child order",
		"parent_order_id", child.ParentID, "order_id", child.OrderID,
		"slice", child.Index, "quantity", child.Quantity.FloatString(12),
		"due_at", child.Due, "late_by", now.Sub(child.Due).Round(time.Second))
	return nil
}

// cancelChildren withdraws every live child of a parent being cancelled, and
// reports the quantity that was never sent (#435).
//
// # This is the other half of clause (c)
//
// schedule.Due stops UNSENT children — that is #435's assertion, and it is
// satisfied by a terminal parent yielding nothing. It says nothing about the
// slices already at a venue, and a cancel that stopped only the future ones
// would leave an operator who pulled a 60-unit order with 30 units still working
// at an exchange and no way to see why. Pulling a parent means pulling the whole
// decision.
//
// # Each child is withdrawn through the ordinary cancel path
//
// handleCancel is the one place an order is withdrawn on this platform: it takes
// the per-order claim, validates against the aggregate, dispatches to the venue
// BEFORE recording the cancellation, and announces. A second withdrawal path for
// children would be a second answer to "what is a cancelled order".
//
// Recursion terminates at depth one: validateSchedule refuses a command carrying
// both a parent and a schedule, so a child can never itself be a parent.
//
// # A child that cannot be withdrawn stops the parent's cancel
//
// Returned as an error, so the whole cancel nacks and redelivers rather than the
// parent being marked CANCELLED over a slice still live at an exchange.
func (s *Service) cancelChildren(ctx context.Context, parentSt *orderpb.OrderState, now time.Time) (*commonpb.Decimal, error) {
	children, err := s.store.ListByParent(ctx, parentSt.GetOrderId())
	if err != nil {
		return nil, fmt.Errorf("oms: could not read the children of order %s to withdraw them: %w",
			parentSt.GetOrderId(), err)
	}

	// THE UNSENT QUANTITY IS THE PARENT'S TOTAL LESS WHAT ITS CHILDREN CARRY —
	// derived from the children that EXIST, not from a slice count, because a
	// parent cancelled mid-schedule has sent a prefix and the rest was never
	// created. Computed before any withdrawal, from the same rows the withdrawal
	// walks, so the number reported is the one that was true when the cancel
	// arrived.
	unsent := dec.FromProto(parentSt.GetOrderedQuantity())
	for _, child := range children {
		unsent.Sub(unsent, dec.FromProto(child.GetOrderedQuantity()))
	}
	if unsent.Sign() < 0 {
		// Every slice was sent and then some — which cannot happen unless the
		// children disagree with the schedule that derived them. Report zero
		// rather than a negative withdrawal, and say so.
		s.logger.Error("oms: a parent's children carry more quantity than the parent — "+
			"reporting nothing withdrawn rather than a negative quantity",
			"order_id", parentSt.GetOrderId(), "children", len(children))
		unsent = new(big.Rat)
	}

	for _, child := range children {
		if IsTerminal(child) {
			continue // already finished; nothing to withdraw
		}
		payload, merr := proto.Marshal(&orderpb.CancelOrder{
			Metadata: &commandpb.CommandMetadata{
				TargetId: child.GetOrderId(),
				Issuer:   ScheduleIssuer,
				Reason:   fmt.Sprintf("parent order %s was cancelled", parentSt.GetOrderId()),
				// THE PARENT'S PORTFOLIO, AND IT IS REQUIRED RATHER THAN OPTIONAL.
				//
				// handleCancel's entitlement check is entitledTo, not
				// delegatedAndEntitled: it does NOT exempt a non-user issuer, and
				// PortfolioEntitled treats an EMPTY list as entitled to nothing.
				// So a platform-issued cancel carrying no scope is refused
				// NOT_ENTITLED — every child would silently survive its parent's
				// cancellation.
				//
				// The scope is the parent's own portfolio, which is the authority
				// this withdrawal acts under: the operator who cancelled the
				// parent was entitled to it, and a child trades nothing else.
				PrincipalPortfolios: []string{parentSt.GetPortfolioId()},
			},
			OrderId: child.GetOrderId(),
		})
		if merr != nil {
			return nil, fmt.Errorf("oms: could not build the withdrawal for child %s: %w",
				child.GetOrderId(), merr)
		}
		if cerr := s.handleCancel(ctx, payload); cerr != nil {
			return nil, fmt.Errorf("oms: could not withdraw child %s of cancelled parent %s: %w",
				child.GetOrderId(), parentSt.GetOrderId(), cerr)
		}

		// A NIL RETURN FROM handleCancel DOES NOT MEAN THE CHILD WAS CANCELLED,
		// and this re-read is the difference between a fan-out and the appearance
		// of one.
		//
		// handleCancel ANSWERS a refusal — NOT_ENTITLED, ORDER_QUARANTINED,
		// ORDER_TERMINAL — by publishing a REJECTED outcome and returning nil,
		// because from the bus's point of view the command was handled. Trusting
		// that nil would let the parent be marked CANCELLED while its slices went
		// on filling at an exchange, with a successful-looking cancel in the log
		// and nothing anywhere saying otherwise. That is the precise shape of
		// failure this platform refuses to ship: a control that reports success.
		after, _, lerr := s.store.Load(ctx, child.GetOrderId())
		if lerr != nil {
			return nil, fmt.Errorf("oms: could not confirm the withdrawal of child %s: %w",
				child.GetOrderId(), lerr)
		}
		if !IsTerminal(after) {
			return nil, fmt.Errorf("oms: child %s of cancelled parent %s is still %s after being "+
				"withdrawn — the cancel was refused, and marking the parent cancelled now would "+
				"report a withdrawal that did not happen while this slice is still live at a venue",
				child.GetOrderId(), parentSt.GetOrderId(), after.GetStatus())
		}
	}

	qty, ok := dec.ToProtoScaled(unsent)
	if !ok {
		return nil, fmt.Errorf("oms: the unsent quantity of order %s (%s) cannot be represented "+
			"as a decimal", parentSt.GetOrderId(), unsent.FloatString(20))
	}
	return qty, nil
}

// TightestSliceInterval reports the shortest gap between two consecutive slices
// across every parent this OMS is currently working, and how many parents it
// looked at.
//
// IT EXISTS SO THE DRIVER'S TICK CAN BE COMPARED TO THE WORK IT IS DRIVING. A
// child is sent on the first tick at or after it is due, so a five-minute tick
// working a one-minute schedule silently turns a 60-slice TWAP into a 12-slice
// one. Every slice still goes out, the quantities still sum, no error is raised
// anywhere — the order simply was not worked the way it was asked to be, and the
// only evidence is in fill timestamps nobody is reading.
//
// ok is false when no parent is being worked, which is NOT an interval of zero.
func (s *Service) TightestSliceInterval(ctx context.Context) (gap time.Duration, parents int, ok bool) {
	working, err := s.store.ListByStatus(ctx, orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED)
	if err != nil {
		return 0, 0, false
	}
	for _, st := range working {
		p, perr := parentOf(st)
		if perr != nil || p.Plan.Slices <= 1 {
			// A single-slice parent has no gap between slices, so it constrains
			// nothing — it is not an interval of zero.
			continue
		}
		parents++
		step := p.Plan.End.Sub(p.Plan.Start) / time.Duration(p.Plan.Slices)
		if !ok || step < gap {
			gap, ok = step, true
		}
	}
	return gap, parents, ok
}
