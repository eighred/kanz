package order

import (
	"testing"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
)

// A COLLATERAL REGIME THE VENUE CANNOT WORK IS REFUSED AT ADMISSION (#417).
//
// This is where the leverage refusal LIVES NOW. internal/signal/translate used
// to refuse leverage != 1 and any margin mode outright, because
// order.v1.SubmitOrder had nowhere to carry them — the #240 stopgap. That was a
// hardcode: it answered the same way for every venue, forever, and it could not
// say which venue might have taken the order.
//
// The order now carries both terms and the adapter declares which regimes it can
// express, so the refusal is a capability contract. It has NOT weakened: both
// spot connectors declare CASH only, so a levered order is refused today exactly
// as it was — but the refusal names the venue, and it retires itself the day a
// margined connector declares CROSS.
//
// WHY THIS IS THE WORST MEMBER OF ITS DEFECT FAMILY if it were missing. #405's
// stop was accepted and inert: it did nothing. #486's IOC rested: it did the
// wrong thing, and the trader held exposure they had asked not to. A margin_mode
// silently dropped places a REAL order at a REAL size with the collateral regime
// absent — the position is live, the audit root records 10x cross, the venue
// holds it as spot, and nothing downstream can tell, because both records are
// internally consistent with themselves.
func TestMarginModeTheVenueCannotWork_IsRejected(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()

	// A venue declaring CASH only, exactly as both live adapters now do.
	spot := execution.WithMarginModes(execution.NewSimVenue("XBIN"),
		[]orderpb.MarginMode{orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED})
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{spot}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := testCtx()

	// A WELL-FORMED order whose only fault is its collateral regime: priced, sized
	// and of a type the sim venue places. Without that, it could be refused for an
	// unrelated reason and this test would pass with the gate never running.
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "XBIN"
	cmd.MarginMode = orderpb.MarginMode_MARGIN_MODE_CROSS
	cmd.Leverage = d(10, 0)
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if _, _, lerr := store.Load(ctx, cmd.GetOrderId()); lerr == nil {
		t.Fatal("the order was ADMITTED under a regime its venue cannot work.\n" +
			"It would be placed as SPOT while the audit root records 10x cross — a live position " +
			"whose collateral regime the fund's own records get wrong, with both records " +
			"internally consistent so nothing downstream can notice.")
	}
	oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc == nil || oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("command outcome = %v, want REJECTED", oc.GetStatus())
	}
	if oc.GetErrorCode() != "MARGIN_MODE_NOT_SUPPORTED" {
		t.Errorf("rejection code = %q, want MARGIN_MODE_NOT_SUPPORTED", oc.GetErrorCode())
	}
	for _, ev := range fb.types() {
		if ev == EventTypeRouted || ev == EventTypeFilled {
			t.Fatalf("the order was %s despite a regime the venue cannot work", ev)
		}
	}
}

// LEVERAGE WITH NO MARGIN MODE IS SPOT CLAIMING TO BE LEVERED, and it is refused
// with no venue in the question at all.
//
// This is the arm that stops #417 from restoring #240 through its own fix. Every
// connector declares CASH, so MARGIN_MODE_UNSPECIFIED is supported EVERYWHERE —
// meaning leverage 10 with no regime named sails through the venue gate above,
// gets placed as an ordinary spot order, and leaves the audit root asserting
// 10x. Exactly the defect the whole change exists to retire.
func TestLeverageWithoutAMarginMode_IsRejected(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	spot := execution.WithMarginModes(execution.NewSimVenue("XBIN"),
		[]orderpb.MarginMode{orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED})
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{spot}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := testCtx()

	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "XBIN"
	cmd.Leverage = d(10, 0) // and deliberately NO margin mode
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if _, _, lerr := store.Load(ctx, cmd.GetOrderId()); lerr == nil {
		t.Fatal("an order asking for 10x with no collateral regime was ADMITTED — spot is " +
			"unlevered, so it would be placed unlevered while the audit root recorded 10x")
	}
	oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc == nil || oc.GetErrorCode() != "LEVERAGE_WITHOUT_MARGIN_MODE" {
		t.Fatalf("rejection code = %q, want LEVERAGE_WITHOUT_MARGIN_MODE", oc.GetErrorCode())
	}
}

// UNLEVERED SPOT IS UNAFFECTED, in all three spellings a caller can write it.
//
// Without this the two tests above pass just as well against a gate that refuses
// everything — and refusing everything is the failure mode the "empty means did
// not say" contract exists to prevent, since every adapter in the estate answered
// nothing about margin until #417 shipped.
func TestUnleveredSpotIsAdmittedEveryWay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*orderpb.SubmitOrder)
	}{
		{"no leverage, no mode", func(*orderpb.SubmitOrder) {}},
		{"explicit leverage 1", func(c *orderpb.SubmitOrder) { c.Leverage = d(1, 0) }},
		{"explicit spot mode", func(c *orderpb.SubmitOrder) {
			c.MarginMode = orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED
			c.Leverage = d(1, 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeBus{}
			store := NewMemoryStore()
			spot := execution.WithMarginModes(execution.NewSimVenue("XBIN"),
				[]orderpb.MarginMode{orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED})
			svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
				execution.NewRouter([]execution.Venue{spot}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
			if err != nil {
				t.Fatal(err)
			}
			ctx := testCtx()
			cmd := limitOrder(d(100, 0), d(1025, -2))
			cmd.Venue = "XBIN"
			tc.apply(cmd)
			if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if _, _, lerr := store.Load(ctx, cmd.GetOrderId()); lerr != nil {
				oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
				t.Fatalf("unlevered spot was REFUSED (%q) — this is what the platform places, so "+
					"refusing it is a trading outage caused by a schema addition",
					oc.GetErrorCode())
			}
		})
	}
}

// AN ADAPTER THAT DECLARED NOTHING STILL TAKES ORDERS. Every adapter in the
// estate answered nothing about margin until #417, so reading silence as
// "supports no regime" would have refused every order the platform places.
func TestAnAdapterThatDeclaredNoMarginModeIsNotGated(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	// NOT wrapped: the adapter never answered the question.
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{execution.NewSimVenue("XBIN")}), nil, nil,
		WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := testCtx()
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "XBIN"
	cmd.MarginMode = orderpb.MarginMode_MARGIN_MODE_CROSS
	cmd.Leverage = d(10, 0)
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, _, lerr := store.Load(ctx, cmd.GetOrderId()); lerr != nil {
		oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
		t.Fatalf("an undeclared adapter refused a margin mode (%q) — empty means DID NOT SAY, "+
			"never SUPPORTS NOTHING, or a schema addition becomes a trading outage",
			oc.GetErrorCode())
	}
}
