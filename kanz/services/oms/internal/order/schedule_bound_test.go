package order

import (
	"strings"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// A SCHEDULE FINER THAN THE DRIVER'S TICK IS REFUSED AT ADMISSION (#898).
//
// The window in these tests is one hour (schedStart..schedEnd) and the driver
// ticks every DefaultScheduleInterval (10s), so 360 slices is the most that can
// be sent when it is due. Above that, several children land on each tick: every
// slice still goes out and the quantities still sum, so nothing errors — the
// parent is simply worked more coarsely than the caller asked for, while every
// screen shows the schedule they specified.
//
// The platform already knew this. Service.TightestSliceInterval computes exactly
// this comparison on every pass and raises scheduleTickTooSlow. #898 is what
// happens when nobody acts on it at the door: slice_count is a uint32 off the
// wire, validateSchedule DERIVES the plan at admission to see it refuse, and the
// derivation allocates one Slice per count — about 160 GiB at the top of the
// range, on the admission path, from one command.

func TestAScheduleTighterThanTheDriverTickIsRefused(t *testing.T) {
	at := schedStart
	fb := &fakeBus{}
	svc, _ := scheduledService(t, fb, &at)

	// 3,600 slices over an hour is one child per second, against a 10s tick.
	cmd := scheduledOrder("ptight", 3600, nil)
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	rej, ok := fb.last(EventTypeRejected).(*orderpb.OrderRejected)
	if !ok {
		t.Fatalf("no ORDER_REJECTED published: %v — a schedule the driver cannot send on time "+
			"was ADMITTED. It would rest at WORKING_SCHEDULED looking exactly like an order "+
			"being worked as asked", fb.types())
	}
	if rej.GetErrorCode() != "INVALID_SCHEDULE" {
		t.Errorf("error code = %q, want INVALID_SCHEDULE", rej.GetErrorCode())
	}
	// THE REFUSAL MUST NAME WHAT TO CHANGE. An operator told only "invalid" has
	// two knobs and no way to choose between them.
	for _, want := range []string{"360", "OMS_SCHEDULE_INTERVAL"} {
		if !strings.Contains(rej.GetReason(), want) {
			t.Errorf("reason does not mention %q, so the caller is not told which slice count "+
				"would work or which knob moves the limit.\n\ngot: %s", want, rej.GetReason())
		}
	}
}

// THE PATHOLOGICAL COUNT IS REFUSED WITHOUT DERIVING THE SCHEDULE, which is the
// half that matters for the process rather than for the caller.
//
// math.MaxUint32 slices is ~160 GiB of []Slice. The bound runs BEFORE
// planFromOrder, so this must come back as a refusal rather than as an
// out-of-memory kill — and a test that merely asserted the refusal could pass
// while the allocation still happened first, so the ordering is what is really
// under test here: if this returns at all, nothing tried to allocate.
func TestAnAbsurdSliceCountIsRefusedRatherThanAllocated(t *testing.T) {
	at := schedStart
	fb := &fakeBus{}
	svc, _ := scheduledService(t, fb, &at)

	cmd := scheduledOrder("pabsurd", 4294967295, nil) // math.MaxUint32
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	rej, ok := fb.last(EventTypeRejected).(*orderpb.OrderRejected)
	if !ok {
		t.Fatalf("no ORDER_REJECTED for a MaxUint32 slice count: %v", fb.types())
	}
	if rej.GetErrorCode() != "INVALID_SCHEDULE" {
		t.Errorf("error code = %q, want INVALID_SCHEDULE", rej.GetErrorCode())
	}
}

// Order ids are letters and digits only: UsableParentID refuses a hyphen because
// the order id is stamped directly as the venue clOrdId and OKX accepts nothing
// else. A hyphenated id here would make every case below refuse for that reason
// instead, which is how a bound test passes while testing nothing.

// A DRIVABLE SCHEDULE IS STILL ADMITTED, and this is the arm that stops the
// bound being tightened into an outage. Exactly at the tick is workable: the
// comparison is gap >= tick, not gap > tick, because a child due exactly on a
// tick is sent by that tick.
func TestAScheduleExactlyAtTheDriverTickIsAdmitted(t *testing.T) {
	at := schedStart
	fb := &fakeBus{}
	svc, _ := scheduledService(t, fb, &at)

	cmd := scheduledOrder("pexact", 360, nil) // one hour / 10s
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if rej, ok := fb.last(EventTypeRejected).(*orderpb.OrderRejected); ok {
		t.Fatalf("a schedule at exactly the driver tick was REFUSED: %s — the bound is one "+
			"slice too strict, and every parent sized to the tick is now rejected",
			rej.GetReason())
	}
}

// AN UNWIRED SERVICE STILL REFUSES. WithScheduleInterval is a composition-root
// option, and a missing option must not read as "unbounded" — that is the same
// "nothing configured looks like checked, and fine" failure the rest of this
// service refuses, applied to the one check standing in front of a 160 GiB
// allocation.
func TestAServiceWithNoConfiguredIntervalStillRefusesAnUndrivableSchedule(t *testing.T) {
	var svc Service // zero value: scheduleInterval is 0

	cmd := scheduledOrder("punwired", 3600, nil)
	rej := svc.refuseUndrivableSchedule(cmd, cmd.GetExecutionSchedule())
	if rej == nil {
		t.Fatal("a Service built without WithScheduleInterval admitted a schedule 360× finer " +
			"than the driver tick. A zero interval must read as DefaultScheduleInterval, never " +
			"as no bound at all")
	}
}
