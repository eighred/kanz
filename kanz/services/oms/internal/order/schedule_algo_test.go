package order

import (
	"context"
	"errors"
	"strings"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution/algo"
)

// AN ORDER NAMES THE ALGORITHM THAT WORKS IT, AND AN UNKNOWN NAME IS REFUSED
// UNDER ITS OWN CODE (#868).
//
// schedule_drive_test.go already proved that a schedule naming
// EXECUTION_ALGO_UNSPECIFIED is not admitted. This file proves the two things
// that were NOT true before the registry landed:
//
//   - the refusal carries UNKNOWN_EXECUTION_ALGO rather than INVALID_SCHEDULE, so
//     a client can tell "your numbers are wrong" from "this build cannot serve
//     you", which on a rolling deploy is the difference between fixing the order
//     and retrying it;
//   - the wire enum reaches the registry by DERIVATION rather than through a
//     switch, so an enum value this build has never seen — the case that arrives
//     when a newer producer talks to an older pod — is refused rather than
//     silently read as the zero value and worked as something.
//
// The failure being guarded against is not a crash. It is an order labelled VWAP
// that TWAP quietly worked: the fills arrive, the parent completes, and every
// reader of the execution record — attribution, TCA, the operator explaining the
// day — is reading the name of an algorithm that never ran.

// ===== THE WIRE ENUM REACHES THE REGISTRY =====

// THE ENUM'S NAME IS THE ALGORITHM'S NAME, WITH THE PREFIX REMOVED. Deriving it
// is what keeps the wire vocabulary and the registry's vocabulary from becoming
// two lists that drift; a switch here would compile perfectly while refusing an
// algorithm the platform had already implemented.
func TestScheduleAlgo_TheWireEnumIsResolvedByDerivationNotASwitch(t *testing.T) {
	if got := algoNameOf(orderpb.ExecutionAlgo_EXECUTION_ALGO_TWAP); got != algo.NameTWAP {
		t.Fatalf("algoNameOf(EXECUTION_ALGO_TWAP) = %q, want %q", got, algo.NameTWAP)
	}
	if _, err := algo.Lookup(algoNameOf(orderpb.ExecutionAlgo_EXECUTION_ALGO_TWAP)); err != nil {
		t.Fatalf("the enum every scheduled order on this platform carries does not resolve: %v", err)
	}

	// AND EVERY OTHER VALUE FAILS CLOSED, including one no schema names. An old
	// pod reading a newer producer's enum gets digits from String(), which carries
	// no prefix, which is not a registered name — so it is refused rather than
	// defaulted to the zero value and worked as whatever that happens to be.
	for _, tt := range []struct {
		name string
		in   orderpb.ExecutionAlgo
	}{
		{"unspecified", orderpb.ExecutionAlgo_EXECUTION_ALGO_UNSPECIFIED},
		{"a value no schema in this build names", orderpb.ExecutionAlgo(99)},
		{"a negative value", orderpb.ExecutionAlgo(-1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := algo.Lookup(algoNameOf(tt.in)); !errors.Is(err, algo.ErrUnknownAlgo) {
				t.Fatalf("algoNameOf(%v) resolved to a working algorithm (err = %v) — an order "+
					"nobody can attribute would be worked", tt.in, err)
			}
		})
	}
}

// ===== THE REFUSAL, AT ADMISSION, UNDER ITS OWN CODE =====

// AN UNIMPLEMENTED ALGORITHM IS REFUSED WITH UNKNOWN_EXECUTION_ALGO, and the
// order does not exist afterwards.
//
// Both halves matter. The code is what tells the client which kind of problem
// they have; the absent order is what stops a parent resting at
// WORKING_SCHEDULED, looking live on every screen, while nothing can ever slice
// it — "nothing configured" and "checked, and fine" looking identical, which is
// the state this platform refuses to allow.
func TestScheduleE2E_AnUnimplementedAlgorithmIsRefusedUnderItsOwnCode(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   orderpb.ExecutionAlgo
	}{
		{"unspecified", orderpb.ExecutionAlgo_EXECUTION_ALGO_UNSPECIFIED},
		{"a value this build does not name", orderpb.ExecutionAlgo(99)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			at := schedStart
			fb := &fakeBus{}
			svc, store := scheduledService(t, fb, &at)

			cmd := scheduledOrder("p1", 6, nil)
			cmd.ExecutionSchedule.Algo = tt.in
			if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
				t.Fatalf("Handle: %v", err)
			}

			// A HANDLER RETURNING NIL MEANS ANSWERED, NOT ACCEPTED — it acks and
			// publishes the refusal. The store is what says whether an order exists.
			if _, _, err := store.Load(context.Background(), "p1"); err == nil {
				t.Fatal("an order naming an algorithm this build cannot work was ADMITTED — it " +
					"would rest at WORKING_SCHEDULED forever, and the driver would refuse it on " +
					"every tick with nobody watching the log")
			}

			msg := fb.last(EventTypeRejected)
			if msg == nil {
				t.Fatalf("no ORDER_REJECTED published: %v", fb.types())
			}
			rej, ok := msg.(*orderpb.OrderRejected)
			if !ok {
				t.Fatalf("ORDER_REJECTED payload is %T", msg)
			}
			if rej.GetErrorCode() != ReasonUnknownExecutionAlgo {
				t.Errorf("error code = %q, want %q — INVALID_SCHEDULE says the caller's own "+
					"numbers are wrong and they should change the command; this says the command "+
					"is fine and this build cannot serve it, and a client retrying is right in "+
					"one case and wrong in the other",
					rej.GetErrorCode(), ReasonUnknownExecutionAlgo)
			}
			// THE REFUSAL NAMES WHAT THIS BUILD DOES IMPLEMENT. Without it an
			// operator on a half-rolled deploy cannot tell a typo from a version skew.
			if !strings.Contains(rej.GetReason(), string(algo.NameTWAP)) {
				t.Errorf("reason %q does not say which algorithms this build implements",
					rej.GetReason())
			}
		})
	}
}

// A WORKABLE SCHEDULE'S REFUSALS KEEP THEIR OWN CODE. The naming refusal must not
// have swallowed the arithmetic one: an operator whose cap and slice count
// disagree needs to be told that, not that their algorithm does not exist.
func TestScheduleE2E_AnUnworkablePlanIsStillINVALIDSCHEDULE(t *testing.T) {
	at := schedStart
	fb := &fakeBus{}
	svc, _ := scheduledService(t, fb, &at)

	cmd := scheduledOrder("p1", 6, d(5, 0)) // 60 in 6 slices is 10 each, over a cap of 5
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	rej, ok := fb.last(EventTypeRejected).(*orderpb.OrderRejected)
	if !ok {
		t.Fatalf("no ORDER_REJECTED published: %v", fb.types())
	}
	if rej.GetErrorCode() != "INVALID_SCHEDULE" {
		t.Errorf("error code = %q, want INVALID_SCHEDULE — the plan is unworkable, and telling "+
			"the caller their algorithm does not exist would send them to fix the wrong thing",
			rej.GetErrorCode())
	}
}

// ===== THE DRIVER REFUSES ONE THAT REACHED THE STORE ANYWAY =====

// A STORED PARENT NAMING AN UNKNOWN ALGORITHM IS REFUSED LOUDLY BY THE DRIVER.
//
// Admission is the first line, not the only one: a parent could have been
// admitted by a build that implemented an algorithm this one does not, which is
// exactly what a rollback looks like. Silence here would leave that order resting
// forever, so the driver must return the reason rather than skip it.
func TestScheduleDriver_AStoredParentNamingAnUnknownAlgorithmIsRefusedLoudly(t *testing.T) {
	at := schedStart
	fb := &fakeBus{}
	svc, store := scheduledService(t, fb, &at)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 6, nil))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	st, _, err := store.Load(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The rollback, simulated on the stored order: a schedule naming something
	// this build cannot resolve.
	st.ExecutionSchedule.Algo = orderpb.ExecutionAlgo(99)

	if _, err := parentOf(st); err != nil {
		t.Fatalf("parentOf refused before the driver could report the reason: %v", err)
	}
	if _, err := svc.driveOne(driveCtx(), st, testTenant, schedEnd); err == nil {
		t.Fatal("the driver advanced a parent naming an algorithm it cannot work — the parent " +
			"would rest at WORKING_SCHEDULED with nothing able to slice it and nobody told")
	} else if !errors.Is(err, algo.ErrUnknownAlgo) {
		t.Errorf("err = %v, want algo.ErrUnknownAlgo to survive to the driver so the log names "+
			"the actual problem", err)
	}
}
