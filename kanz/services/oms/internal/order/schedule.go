package order

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution/algo"
	"github.com/eighred/kanz/services/oms/internal/schedule"
)

// The parent/child order relation, at the admission boundary (#435).
//
// internal/execution/algo derives the schedule; services/oms/internal/schedule
// decides which children should exist. This file is where those meet the order
// aggregate: turning a stored parent into a Plan, and deciding whether a
// SubmitOrder claiming to be a child is really one.

// ReasonUnknownExecutionAlgo refuses a schedule naming an algorithm this build
// cannot work (#868).
//
// ITS OWN CODE, NOT INVALID_SCHEDULE, and the distinction is operational rather
// than cosmetic. INVALID_SCHEDULE means the operator's own numbers do not work —
// a window that does not move forward, a cap the slice count cannot honour — and
// they fix it by changing the command. This means the COMMAND IS FINE AND THIS
// BUILD CANNOT SERVE IT, which on a rolling deploy is the difference between a
// bad order and a fleet where half the pods can work it. A client retrying the
// same command is right in the second case and wrong in the first, and one code
// cannot tell them apart.
const ReasonUnknownExecutionAlgo = "UNKNOWN_EXECUTION_ALGO"

// algoNameOf translates order.v1's wire enum to the core's algorithm name.
//
// DERIVED FROM THE ENUM'S OWN NAME, NOT A SWITCH. A switch is a second list: add
// EXECUTION_ALGO_VWAP to the schema and implement VWAP in internal/execution/algo
// and the order path still refuses it until somebody remembers a third edit here.
// Stripping the enum prefix makes the wire vocabulary and the registry's
// vocabulary the same vocabulary, so there is exactly one list — Registered() —
// and this function cannot drift from it.
//
// EVERY FAILURE OF THIS MAPPING FAILS CLOSED. UNSPECIFIED yields "UNSPECIFIED",
// and a numeric value no schema names at all (an old pod reading a newer
// producer's enum) yields "", because String() renders it as digits with no
// prefix. Both reach algo.Lookup, and Lookup refuses both. There is no path
// through here that reaches an algorithm the caller did not name.
func algoNameOf(a orderpb.ExecutionAlgo) algo.Name {
	const prefix = "EXECUTION_ALGO_"
	name := a.String()
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	return algo.Name(strings.TrimPrefix(name, prefix))
}

// planFromOrder derives a parent's schedule from its durable fields.
//
// EVERY INPUT COMES OFF THE ORDER, which is what makes the schedule survive a
// restart without being stored: two pods holding the same order derive the same
// children, forever. Nothing here reads a clock or a store.
//
// THE ALGORITHM IS ONE OF THOSE FIELDS (#868), carried onto the Plan rather than
// checked here. It used to be checked here, against TWAP by name; the check has
// not been dropped but MOVED, to algo.Lookup, which is now the single place a
// name becomes an implementation. Refusing in two places is how the two answers
// drift, and the surviving one is the registry's because it is also what the
// arch guard holds complete.
func planFromOrder(st *orderpb.OrderState) (algo.Plan, error) {
	sch := st.GetExecutionSchedule()
	if sch == nil {
		return algo.Plan{}, fmt.Errorf("%w: order %s", schedule.ErrNoSchedule, st.GetOrderId())
	}
	p := algo.Plan{
		Algo:   algoNameOf(sch.GetAlgo()),
		Total:  dec.FromProto(st.GetOrderedQuantity()),
		Start:  sch.GetWindowStart().AsTime().UTC(),
		End:    sch.GetWindowEnd().AsTime().UTC(),
		Slices: int(sch.GetSliceCount()),
	}
	if cap := sch.GetMaxSliceQuantity(); cap != nil {
		p.MaxSlice = dec.FromProto(cap)
	}
	return p, nil
}

// parentOf reduces a stored parent order to the decision's view of it.
//
// TERMINAL IS COMPUTED FROM THE STORED STATUS, here and nowhere else, so #435's
// clause (c) cannot be lost by a caller that forgets to pass it. IsTerminal is
// the aggregate's own predicate — the same one every other transition consults.
func parentOf(st *orderpb.OrderState) (schedule.Parent, error) {
	plan, err := planFromOrder(st)
	if err != nil {
		return schedule.Parent{}, err
	}
	return schedule.Parent{
		OrderID:  st.GetOrderId(),
		Terminal: IsTerminal(st),
		Plan:     plan,
	}, nil
}

// validateSchedule refuses a SubmitOrder whose schedule cannot be worked, AT
// ADMISSION — before the order exists.
//
// EVERY ONE OF THESE WOULD OTHERWISE BECOME A PARENT THAT RESTS FOREVER. It
// would be admitted, announced, shown working on every screen, and the driver
// would refuse it on every tick with nobody watching the log. "Nothing
// configured" and "checked, and fine" must never look the same, and an order
// resting at WORKING_SCHEDULED looks exactly like one being worked.
func validateSchedule(cmd *orderpb.SubmitOrder) *RejectError {
	sch := cmd.GetExecutionSchedule()
	if sch == nil {
		return nil // an ordinary order; nothing to validate
	}
	// A CHILD MAY NOT ITSELF BE SCHEDULED. Nesting would make ChildID ambiguous
	// (see schedule.UsableParentID) and would let one command fan out without
	// bound — a schedule of schedules of schedules, each multiplying the last.
	if cmd.GetParentOrderId() != "" {
		return reject("INVALID_SCHEDULE",
			"order %s is a child of %s and cannot itself be worked as a schedule: nesting has no "+
				"bound and would make child ids ambiguous",
			cmd.GetOrderId(), cmd.GetParentOrderId())
	}
	if err := schedule.UsableParentID(cmd.GetOrderId()); err != nil {
		return reject("INVALID_SCHEDULE", "%s", err.Error())
	}
	// THE SCHEDULE IS DERIVED HERE, AT ADMISSION, PURELY TO SEE IT REFUSE. The
	// same arithmetic the driver will run every tick — so a window that does not
	// move forward, a slice count of zero, or a cap the slices cannot honour is
	// the caller's answer to their own command rather than a log line hours later.
	//
	// It is derived THROUGH THE REGISTRY (#868), which is also what refuses an
	// algorithm this build does not implement. One call, two refusals, and they
	// stay distinguishable: algo.ErrUnknownAlgo says the name is the problem, and
	// anything else says the numbers are. Nothing here compares the algorithm
	// against a name of its own, so this admission check cannot fall out of step
	// with what the driver will actually be able to work.
	plan := algo.Plan{
		Algo:   algoNameOf(sch.GetAlgo()),
		Total:  dec.FromProto(cmd.GetQuantity()),
		Start:  sch.GetWindowStart().AsTime().UTC(),
		End:    sch.GetWindowEnd().AsTime().UTC(),
		Slices: int(sch.GetSliceCount()),
	}
	if cap := sch.GetMaxSliceQuantity(); cap != nil {
		plan.MaxSlice = dec.FromProto(cap)
	}
	// NOTHING IS SENT AND NOTHING IS KNOWN ABOUT THE MARKET, and both are said
	// rather than left blank. The order does not exist yet, so no slice of it can
	// have been sent — that is a KNOWN "none", not an unknown. The OMS has no
	// market data on this path at all, so UnknownMarket is the honest answer, and
	// an algorithm needing a book or a volume profile (#867) refuses the order
	// here rather than being handed a zero.
	state := algo.ParentState{OrderID: cmd.GetOrderId(), Sent: func(int) bool { return false }}
	if _, err := algo.Run(plan, state, algo.UnknownMarket{}); err != nil {
		if errors.Is(err, algo.ErrUnknownAlgo) {
			return reject(ReasonUnknownExecutionAlgo,
				"the schedule names how order %s is worked and must not be defaulted: %s",
				cmd.GetOrderId(), err.Error())
		}
		return reject("INVALID_SCHEDULE", "%s", err.Error())
	}
	return nil
}

// authorizeChild decides whether a SubmitOrder claiming a parent really is that
// parent's slice, and returns the parent it belongs to.
//
// # The schedule IS the authorization
//
// parent_order_id is on the command because children are admitted through the
// ordinary command path — one admission implementation, not a second one for
// slices. That raises the obvious question: what stops a client setting it?
//
// NOT AN IDENTITY CHECK. A child is admitted if and only if it is EXACTLY a
// slice the parent's own durable schedule derives: the right id, from the right
// parent, for the right instrument and side, in the right quantity. Anything
// else is refused. So a client that forges one can only ever produce the order
// the driver was going to send anyway — which the store's primary key then
// deduplicates. There is no privilege to steal, because being the driver confers
// none.
//
// That is stronger than trusting an issuer, and it survives the case an issuer
// check does not: a driver with a bug cannot send a child the schedule does not
// describe.
//
// # A terminal parent refuses its children here too
//
// Clause (c) is enforced in schedule.Due, which is where the driver decides. It
// is enforced AGAIN here because the two are separated by a network: a child
// command already in flight when an operator cancels the parent would otherwise
// land after the cancel and trade. This is the check that catches it.
//
// # Three returns, three different things
//
// The three returns are three different things, and collapsing any two of them
// is a defect: a PARENT is an authorized child's parent, a *RejectError is a
// permanent refusal that must be ANSWERED, and an error is a transient failure
// that must be RETRIED. Refusing on a transient failure would reject a
// legitimate child because the database blinked — and the driver would then see
// that slice missing and re-derive it on every tick, against a parent that can
// never complete.
func (s *Service) authorizeChild(ctx context.Context, cmd *orderpb.SubmitOrder) (*orderpb.OrderState, *RejectError, error) {
	parentID := cmd.GetParentOrderId()

	parent, _, err := s.store.Load(ctx, parentID)
	if errors.Is(err, ErrNotFound) {
		return nil, reject("PARENT_NOT_FOUND",
			"order %s claims to be a slice of %s, which this OMS has never admitted",
			cmd.GetOrderId(), parentID), nil
	}
	if err != nil {
		return nil, nil, err // transient ⇒ redeliver
	}

	if parent.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED {
		// COVERS THE CANCEL RACE. A parent an operator cancelled while this child
		// was in flight is no longer WORKING_SCHEDULED, and this is the only thing
		// standing between that command and a venue.
		return nil, reject("PARENT_NOT_WORKING",
			"order %s is a slice of %s, which is %s rather than being worked as a schedule — a "+
				"parent that has stopped must not acquire new children",
			cmd.GetOrderId(), parentID, parent.GetStatus()), nil
	}

	plan, perr := planFromOrder(parent)
	if perr != nil {
		return nil, reject("PARENT_NOT_WORKING",
			"order %s is a slice of %s, whose schedule cannot be derived: %v",
			cmd.GetOrderId(), parentID, perr), nil
	}
	// WHAT HAS BEEN SENT IS UNKNOWN HERE, and Sent is nil to say so rather than a
	// predicate answering false. This runs on ONE inbound child command; it has
	// not read the parent's other children and will not, because doing so would
	// put a second query on the admission path for an answer TWAP does not use.
	// An algorithm that needs to know refuses, which is correct: it must not be
	// told "nothing has been sent" by a caller that never looked.
	slices, terr := algo.Run(plan, algo.ParentState{OrderID: parentID}, algo.UnknownMarket{})
	if terr != nil {
		return nil, reject("PARENT_NOT_WORKING",
			"order %s is a slice of %s, whose schedule is unworkable: %v",
			cmd.GetOrderId(), parentID, terr), nil
	}

	// THE ID MUST BE ONE THE SCHEDULE DERIVES. This is what makes forging a child
	// pointless: the id is a function of the parent and the slice index, so the
	// only ids that pass are the ones the driver would have used.
	want := -1
	for _, sl := range slices {
		if schedule.ChildID(parentID, sl.Index) == cmd.GetOrderId() {
			want = sl.Index
			break
		}
	}
	if want < 0 {
		return nil, reject("NOT_A_SLICE",
			"order %s is not a slice of %s: its schedule has %d slices and none of them is "+
				"named that", cmd.GetOrderId(), parentID, len(slices)), nil
	}

	// AND THE QUANTITY MUST BE THE SLICE'S, EXACTLY. Compared as rationals: the
	// children divide the parent exactly, so a slice of 10/3 is 10/3 and not
	// 3.33333333. A child admitted for the rounded amount would leave the parent
	// unable to complete by the remainder, forever.
	got, ok := dec.FromProtoChecked(cmd.GetQuantity())
	if !ok || got.Cmp(slices[want].Quantity) != 0 {
		return nil, reject("SLICE_QUANTITY_MISMATCH",
			"order %s claims slice %d of %s but carries quantity %s, and that slice is %s",
			cmd.GetOrderId(), want, parentID, quantityString(got), slices[want].Quantity.FloatString(12)), nil
	}

	// THE STATIC TERMS MUST MATCH THE PARENT. A "slice" that buys a different
	// instrument, or sells what the parent buys, is not a slice — it is a
	// different order wearing the parent's authorization, and the parent's
	// compliance check said nothing about it.
	switch {
	case cmd.GetPortfolioId() != parent.GetPortfolioId():
		return nil, reject("SLICE_TERMS_MISMATCH",
			"slice %s names portfolio %q; its parent %s trades %q",
			cmd.GetOrderId(), cmd.GetPortfolioId(), parentID, parent.GetPortfolioId()), nil
	case cmd.GetInstrumentId() != parent.GetInstrumentId():
		return nil, reject("SLICE_TERMS_MISMATCH",
			"slice %s names instrument %q; its parent %s trades %q",
			cmd.GetOrderId(), cmd.GetInstrumentId(), parentID, parent.GetInstrumentId()), nil
	case cmd.GetSide() != parent.GetSide():
		return nil, reject("SLICE_TERMS_MISMATCH",
			"slice %s is a %s; its parent %s is a %s",
			cmd.GetOrderId(), cmd.GetSide(), parentID, parent.GetSide()), nil
	}

	return parent, nil, nil
}

// quantityString renders a quantity for a refusal, including the case where it
// could not be read at all — "unknown" rather than a zero that reads as a real
// number somebody sent.
func quantityString(r *big.Rat) string {
	if r == nil {
		return "unreadable"
	}
	return r.FloatString(12)
}
