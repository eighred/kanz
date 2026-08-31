package order

import (
	"context"
	"math/big"
	"strings"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution/algo"
)

// A VOLUME-DRIVEN ORDER IS REFUSED HERE, AND THE REFUSAL SAYS WHY (#869).
//
// # The state this file records
//
// #869 added VWAP and POV to internal/execution/algo and to order.v1's
// ExecutionAlgo, so an order can NAME either. It did not give this OMS a volume
// profile: all three of this service's entries into the algo package pass
// algo.UnknownMarket, because the OMS holds no market data. So a VWAP or POV
// order is REFUSED at admission today.
//
// THAT IS THE FAIL-CLOSED DIRECTION AND IT IS THE POINT. The alternative — a flat
// curve — is TWAP, so an order labelled VWAP would be worked as TWAP and the fills
// attributed to an algorithm that never ran, which nothing downstream could
// detect. Refusing is visible: the order does not exist, and the client is told.
//
// # It is a THIRD refusal code, not INVALID_SCHEDULE
//
// An operator can now have three different problems with one schedule and each
// needs a different action:
//
//	INVALID_SCHEDULE        your numbers do not work — change the command.
//	UNKNOWN_EXECUTION_ALGO  the command is fine; this BUILD cannot work it.
//	NO_VOLUME_PROFILE       the command is fine and this build implements the
//	                        algorithm; nothing here can SEE the volume it needs.
//
// A client that retried on the first would be wrong; on the third it is right as
// soon as the profile reaches this path. One code cannot tell them apart, which is
// the argument ReasonUnknownExecutionAlgo already made for itself.
//
// WHEN THE PROFILE DOES REACH THIS PATH, THIS FILE IS WHAT MUST CHANGE, and the
// assertions below are written so it fails loudly rather than passing vacuously:
// each one requires the order to be ABSENT from the store, so a build that starts
// admitting these orders fails here rather than quietly widening.

func volumeDrivenOrder(id string, a orderpb.ExecutionAlgo, cap *orderpb.ExecutionSchedule) *orderpb.SubmitOrder {
	cmd := scheduledOrder(id, 6, nil)
	cmd.ExecutionSchedule.Algo = a
	if cap != nil {
		cmd.ExecutionSchedule.MaxParticipationRate = cap.GetMaxParticipationRate()
	}
	return cmd
}

// A VWAP OR POV ORDER IS REFUSED UNDER NO_VOLUME_PROFILE, AND IS NOT ADMITTED.
func TestScheduleE2E_AVolumeDrivenOrderIsRefusedUnderItsOwnCode(t *testing.T) {
	// A cap that makes the POV command WELL-FORMED, so the refusal below is about
	// the missing profile rather than about a missing cap.
	withCap := &orderpb.ExecutionSchedule{MaxParticipationRate: d(8, -2)}

	for _, tt := range []struct {
		name string
		cmd  *orderpb.SubmitOrder
	}{
		{"VWAP", volumeDrivenOrder("p1", orderpb.ExecutionAlgo_EXECUTION_ALGO_VWAP, nil)},
		{"POV with a cap", volumeDrivenOrder("p1", orderpb.ExecutionAlgo_EXECUTION_ALGO_POV, withCap)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			at := schedStart
			fb := &fakeBus{}
			svc, store := scheduledService(t, fb, &at)

			if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, tt.cmd)); err != nil {
				t.Fatalf("Handle: %v", err)
			}

			// A HANDLER RETURNING NIL MEANS ANSWERED, NOT ACCEPTED. The store is
			// what says whether an order exists.
			if _, _, err := store.Load(context.Background(), "p1"); err == nil {
				t.Fatal("a volume-driven order was ADMITTED by an OMS that has no volume profile " +
					"— it would rest at WORKING_SCHEDULED forever, or be worked against a curve " +
					"nobody measured")
			}

			rej, ok := fb.last(EventTypeRejected).(*orderpb.OrderRejected)
			if !ok {
				t.Fatalf("no ORDER_REJECTED published: %v", fb.types())
			}
			if rej.GetErrorCode() != ReasonNoVolumeProfile {
				t.Errorf("error code = %q, want %q — INVALID_SCHEDULE would send the caller to fix "+
					"a command that is correct, and UNKNOWN_EXECUTION_ALGO would tell them this "+
					"build cannot work an algorithm it implements",
					rej.GetErrorCode(), ReasonNoVolumeProfile)
			}
			if !strings.Contains(rej.GetReason(), "volume") {
				t.Errorf("reason %q does not say what is missing", rej.GetReason())
			}
		})
	}
}

// POV WITHOUT A CAP IS INVALID_SCHEDULE, NOT NO_VOLUME_PROFILE.
//
// The two refusals must not collapse into one. A missing cap is the operator's own
// error and they fix it from the command; a missing profile is not theirs at all,
// and a client that retried the first would retry forever. The ordering is the
// algorithm's — pov checks its cap before asking the market anything — and this
// asserts it survives to the client.
func TestScheduleE2E_POVWithoutACapIsTheOperatorsOwnError(t *testing.T) {
	at := schedStart
	fb := &fakeBus{}
	svc, _ := scheduledService(t, fb, &at)

	cmd := volumeDrivenOrder("p1", orderpb.ExecutionAlgo_EXECUTION_ALGO_POV, nil)
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	rej, ok := fb.last(EventTypeRejected).(*orderpb.OrderRejected)
	if !ok {
		t.Fatalf("no ORDER_REJECTED published: %v", fb.types())
	}
	if rej.GetErrorCode() != "INVALID_SCHEDULE" {
		t.Errorf("error code = %q, want INVALID_SCHEDULE", rej.GetErrorCode())
	}
	if !strings.Contains(rej.GetReason(), string(algo.NameVWAP)) {
		t.Errorf("reason %q does not tell the operator which algorithm works this order without "+
			"a cap", rej.GetReason())
	}
}

// THE INSTRUMENT AND THE PARTICIPATION CAP REACH THE PLAN.
//
// Both are new inputs to a schedule (#869) and both come off the ORDER, which is
// what keeps the schedule derivable: two pods holding the same order derive the
// same children. A field validated at admission and then dropped on the way to the
// plan is the defect submit_fields_reach_state_test.go exists for, one layer in —
// and here it would be invisible, because TWAP reads neither.
func TestPlanFromOrder_CarriesTheInstrumentAndTheParticipationCap(t *testing.T) {
	st := &orderpb.OrderState{
		OrderId:         "p1",
		InstrumentId:    "BTC-USD",
		OrderedQuantity: d(60, 0),
		ExecutionSchedule: &orderpb.ExecutionSchedule{
			Algo:                 orderpb.ExecutionAlgo_EXECUTION_ALGO_POV,
			WindowStart:          scheduledOrder("p1", 6, nil).ExecutionSchedule.WindowStart,
			WindowEnd:            scheduledOrder("p1", 6, nil).ExecutionSchedule.WindowEnd,
			SliceCount:           6,
			MaxParticipationRate: d(8, -2),
		},
	}

	plan, err := planFromOrder(st)
	if err != nil {
		t.Fatalf("planFromOrder: %v", err)
	}
	if plan.InstrumentID != "BTC-USD" {
		t.Errorf("plan instrument = %q, want BTC-USD — a volume-driven algorithm would ask the "+
			"market view about nothing", plan.InstrumentID)
	}
	if plan.MaxParticipation == nil {
		t.Fatal("the participation cap did not reach the plan — POV would refuse an order that " +
			"carries one, or worse, work one uncapped if the refusal were ever relaxed")
	}
	if want := new(big.Rat).SetFrac64(8, 100); plan.MaxParticipation.Cmp(want) != 0 {
		t.Errorf("cap = %s, want %s", plan.MaxParticipation.RatString(), want.RatString())
	}
}
