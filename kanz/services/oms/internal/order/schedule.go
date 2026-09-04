package order

import (
	"time"

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

// ReasonNoVolumeProfile refuses a volume-driven schedule this OMS cannot size
// (#869).
//
// ITS OWN CODE, FOR THE REASON ReasonUnknownExecutionAlgo HAS ONE. There are now
// three different problems an operator can have with one schedule, and a client
// that cannot tell them apart retries the wrong one:
//
//	INVALID_SCHEDULE       your numbers do not work — change the command.
//	UNKNOWN_EXECUTION_ALGO the command is fine; this BUILD cannot work it.
//	NO_VOLUME_PROFILE      the command is fine and this build implements the
//	                       algorithm; nothing here can SEE the volume it needs.
//
// The third is not a client error at all. VWAP and POV schedule against #867's
// intraday volume profile, and this admission path passes algo.UnknownMarket —
// the OMS holds no market data — so every volume-driven order is refused here
// today. That is the fail-closed direction and it is deliberately loud: the
// alternative, a flat curve, is TWAP, and an order labelled VWAP that TWAP worked
// is a mislabelled execution the attribution plane cannot detect.
const ReasonNoVolumeProfile = "NO_VOLUME_PROFILE"

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
		Algo: algoNameOf(sch.GetAlgo()),
		// THE INSTRUMENT IS PART OF THE SCHEDULE'S INPUTS NOW (#869), because a
		// volume-driven algorithm asks the market view about a named instrument.
		// It comes off the ORDER like every other input here, so two pods holding
		// the same order still derive the same children.
		InstrumentID: st.GetInstrumentId(),
		Total:        dec.FromProto(st.GetOrderedQuantity()),
		Start:        sch.GetWindowStart().AsTime().UTC(),
		End:          sch.GetWindowEnd().AsTime().UTC(),
		Slices:       int(sch.GetSliceCount()),
	}
	if maxSlice := sch.GetMaxSliceQuantity(); maxSlice != nil {
		p.MaxSlice = dec.FromProto(maxSlice)
	}
	if maxPart := sch.GetMaxParticipationRate(); maxPart != nil {
		p.MaxParticipation = dec.FromProto(maxPart)
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
func (s *Service) validateSchedule(cmd *orderpb.SubmitOrder) *RejectError {
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
	// THE SLICE COUNT IS BOUNDED BEFORE THE SCHEDULE IS DERIVED (#898), and the
	// ordering is the whole point: the derivation below allocates one Slice per
	// slice_count, so a bound applied after it is a bound applied to a process
	// that has already tried to allocate 160 GiB for a single command. slice_count
	// is a uint32 off the wire and nothing else constrains it.
	//
	// THE NUMBER IS DERIVED FROM THE DRIVER, NOT PICKED. A child is sent on the
	// first tick at or after it becomes due, so a schedule whose slices are closer
	// together than OMS_SCHEDULE_INTERVAL cannot be worked the way it was asked
	// for — several children land on one tick and the parent is sliced more
	// coarsely than the caller specified. The platform already KNOWS this: the OMS
	// computes TightestSliceInterval on every pass and warns when the tick is
	// slower than the tightest schedule it is working. This makes that an
	// admission decision instead of a log line, which is what CLAUDE.md asks of
	// this plane — "admission control, not reporting. Decides whether."
	//
	// It closes the allocation as a side effect rather than as its purpose: at a
	// 10s tick a 24h parent tops out at 8,640 slices, four orders of magnitude
	// below the point where the arithmetic in algo.Plan.boundary was overflowing
	// and six below the memory.
	if rej := s.refuseUndrivableSchedule(cmd, sch); rej != nil {
		return rej
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
		Algo:         algoNameOf(sch.GetAlgo()),
		InstrumentID: cmd.GetInstrumentId(),
		Total:        dec.FromProto(cmd.GetQuantity()),
		Start:        sch.GetWindowStart().AsTime().UTC(),
		End:          sch.GetWindowEnd().AsTime().UTC(),
		Slices:       int(sch.GetSliceCount()),
	}
	if maxSlice := sch.GetMaxSliceQuantity(); maxSlice != nil {
		plan.MaxSlice = dec.FromProto(maxSlice)
	}
	if maxPart := sch.GetMaxParticipationRate(); maxPart != nil {
		plan.MaxParticipation = dec.FromProto(maxPart)
	}
	// NOTHING IS SENT, AND THAT IS A KNOWN "none" RATHER THAN AN UNKNOWN. The
	// order does not exist yet, so no slice of it can have been sent.
	state := algo.ParentState{OrderID: cmd.GetOrderId(), Sent: func(int) bool { return false }}

	// THE PIN IS THE PLATFORM'S, NEVER THE CALLER'S, so whatever arrived on the
	// command is discarded before anything reads it. A client that could name the
	// profile version could choose which measured curve its order is sliced
	// against — an older, thinner one gives smaller early children and a shape
	// nobody on the desk selected — and the order's own audit record would then
	// assert a schedule the platform never chose. It is the stance
	// SubmitOrder.venue_account_id takes for the same reason: a field the platform
	// stamps has no business surviving from the wire.
	//
	// THE CURVE IS CLEARED WITH THE VERSION, AND IT IS THE HALF WITH TEETH (#943).
	// A caller who could send the SHAPE would not merely choose among published
	// curves — it would supply one nobody published, and since the two are checked
	// against each other rather than against the feed, a self-consistent pair would
	// verify. Both fields leave on the same line so a future reader cannot repair
	// one and forget the other.
	sch.VolumeProfileVersion = ""
	sch.VolumeProfile = nil

	// THE MARKET IS THE NEWEST PUBLISHED PROFILE FOR THIS (INSTRUMENT, VENUE), AND
	// THIS IS THE ONE PLACE THAT READS "NEWEST" (#897).
	//
	// Admission is the moment the choice is RECORDED, so it is the only moment at
	// which "whatever is current" is a defensible input: every later derivation of
	// this parent's schedule — the child-admission check, the driver tick, the
	// same pod after a restart, a different pod entirely — resolves the version
	// stamped below. Reading current anywhere else would re-plan a working parent
	// against a market it was never sized for.
	//
	// IT IS OFFERED TO EVERY SCHEDULE AND THE ALGORITHM DECIDES. Nothing here asks
	// whether the order is volume-driven, because that question has exactly one
	// correct answer-holder — algo.Registered() — and a copy of it in this file
	// would drift from it. TWAP never asks and is unaffected; VWAP and POV ask,
	// and refuse on UNKNOWN.
	market, profile := s.currentMarket(cmd.GetInstrumentId(), cmd.GetVenue())
	seen := &consultedView{MarketView: market}
	if _, err := algo.Run(plan, state, seen); err != nil {
		if errors.Is(err, algo.ErrUnknownAlgo) {
			return reject(ReasonUnknownExecutionAlgo,
				"the schedule names how order %s is worked and must not be defaulted: %s",
				cmd.GetOrderId(), err.Error())
		}
		if errors.Is(err, algo.ErrVolumeUnknown) {
			// THE ALGORITHM REFUSED RATHER THAN INVENTING A CURVE, which is #869's
			// non-negotiable property arriving at the client under its own code.
			// Reporting it as INVALID_SCHEDULE would send an operator to change a
			// command that is correct.
			return reject(ReasonNoVolumeProfile,
				"order %s is worked by an algorithm that schedules against expected volume, and "+
					"this OMS cannot size it: %s. It is refused rather than worked against a flat "+
					"curve, which would be TWAP under another name. The algorithm's own report: %s",
				cmd.GetOrderId(), s.volumeProfileGap(cmd.GetInstrumentId(), cmd.GetVenue()), err.Error())
		}
		return reject("INVALID_SCHEDULE", "%s", err.Error())
	}

	// THE PIN IS STAMPED ONLY IF THE SCHEDULE ACTUALLY ASKED (#897).
	//
	// A TWAP parent reads no curve, so recording a version on it would assert in
	// the durable audit record that its schedule was sized against a market it
	// never looked at. Whether the view was consulted is OBSERVED at the seam
	// rather than predicted from the algorithm's name — see consultedView — so
	// this cannot drift from what the algorithms actually do.
	//
	// IT MUTATES THE COMMAND, and that is the mechanism rather than a side effect:
	// handleSubmit copies cmd.GetExecutionSchedule() onto the OrderState it
	// commits, so stamping here is what makes the pin durable in the same write
	// that admits the order. There is no second store to fall out of sync with.
	//
	// THE CURVE IS STAMPED WITH THE VERSION, NEVER SEPARATELY (#943). They are one
	// assignment because they are one fact: the shape and the name of the shape.
	// Stamping the version alone is what left a working parent depending on the
	// MARKET stream's 24h retention — resolvable on the pod that admitted it and
	// unresolvable on the pod that replaced it — and stamping the curve alone would
	// store a market under no name at all.
	if seen.asked && profile.GetVersion() != "" {
		sch.VolumeProfileVersion = profile.GetVersion()
		sch.VolumeProfile = profile
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
	//
	// THE MARKET IS THE PARENT'S OWN PINNED PROFILE VERSION, NEVER THE NEWEST
	// (#897). This is the sharpest of the three paths: the quantity comparison
	// below is an EXACT RATIONAL, so a view built here from a curve that has moved
	// since admission derives a different slice and refuses the driver's own child
	// as a forgery — on a parent that then advances no further while every screen
	// shows it working. Resolving the recorded version is what makes this check
	// agree with the derivation that produced the child, on any pod, at any time.
	slices, terr := algo.Run(plan, algo.ParentState{OrderID: parentID}, s.scheduleMarket(parent))
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

// DefaultScheduleInterval mirrors OMS_SCHEDULE_INTERVAL's own default (10s,
// #435). It is the fallback when a Service was built without
// WithScheduleInterval, and it exists so an unwired composition root still
// refuses an undrivable schedule.
//
// A ZERO INTERVAL MUST NOT MEAN "UNBOUNDED". That is the reading which turns a
// missing option into an unbounded allocation on the admission path, and it is
// the same "nothing configured looks like checked, and fine" failure this
// service refuses everywhere else.
const DefaultScheduleInterval = 10 * time.Second

// refuseUndrivableSchedule refuses a slice count the driver cannot honour.
//
// The comparison is the gap between consecutive slices against the tick that
// sends them. It is deliberately the SAME comparison Service.TightestSliceInterval
// feeds to the scheduleTickTooSlow gauge, so an operator who lowers
// OMS_SCHEDULE_INTERVAL raises what this admits, by exactly as much: one number
// governs both, and neither can be tightened without the other following.
func (s *Service) refuseUndrivableSchedule(cmd *orderpb.SubmitOrder, sch *orderpb.ExecutionSchedule) *RejectError {
	tick := s.scheduleInterval
	if tick <= 0 {
		tick = DefaultScheduleInterval
	}
	count := sch.GetSliceCount()
	if count == 0 {
		return nil // ErrNoSlices is the planner's answer, and it names the field
	}
	start, end := sch.GetWindowStart().AsTime().UTC(), sch.GetWindowEnd().AsTime().UTC()
	if !end.After(start) {
		return nil // ErrEmptyWindow is the planner's answer
	}
	// Integer division, so no product is formed and this cannot overflow however
	// large count is — the arithmetic that could is the one downstream of it.
	gap := end.Sub(start) / time.Duration(count)
	if gap >= tick {
		return nil
	}
	return reject("INVALID_SCHEDULE",
		"order %s asks for %d slices across %s, which is one child every %s — closer together "+
			"than the %s driver tick that sends them. Every child would not be sent when it is "+
			"due: several land on each tick, so the parent would be worked more coarsely than "+
			"asked while every screen showed the schedule it specified. Use at most %d slices "+
			"for this window, or lower OMS_SCHEDULE_INTERVAL",
		cmd.GetOrderId(), count, end.Sub(start), gap, tick, int64(end.Sub(start)/tick))
}
