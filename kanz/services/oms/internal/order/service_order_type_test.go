package order

import (
	"testing"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution"
)

// TestOrderTypeTheVenueCannotPlace_IsRejected (#405).
//
// order.v1 declares MARKET, LIMIT, STOP and STOP_LIMIT and Accept validates all
// four, but the spot adapters translate the first two and refuse the rest inside
// Execute. So a stop-loss used to pass admission, be stored, and have its
// ORDER_ACCEPTED FACT committed and published — and only then fail at the venue.
// The order existed everywhere: risk carried exposure for it, tv-sync showed it
// working, the caller had been told it was accepted. It just never went anywhere.
//
// That is TestOrderNamingAnUnconfiguredVenue_IsRejected's defect on a different
// field, discovered one layer later — and for a stop it is the worst possible
// shape, because the whole point of one is to act when nobody is watching, so
// "accepted and inert" is indistinguishable from "armed" until it should fire.
func TestOrderTypeTheVenueCannotPlace_IsRejected(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()

	// A venue that declares spot MARKET and LIMIT, exactly as both live adapters do.
	spot := execution.WithOrderTypes(execution.NewSimVenue("XBIN"), []orderpb.OrderType{
		orderpb.OrderType_ORDER_TYPE_MARKET,
		orderpb.OrderType_ORDER_TYPE_LIMIT,
	})
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter(spot), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testCtx()

	// A WELL-FORMED stop: priced, and carrying its trigger, so Accept has nothing
	// to object to and the ONLY thing wrong with this order is that its venue
	// cannot place it. Without the stop price it is refused as INVALID_PRICE and
	// this test would pass without the gate ever running.
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "XBIN"
	cmd.OrderType = orderpb.OrderType_ORDER_TYPE_STOP_LIMIT
	cmd.StopPrice = d(1000, -2)
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if _, _, lerr := store.Load(ctx, cmd.GetOrderId()); lerr == nil {
		t.Fatal("the order was ADMITTED for a type its venue cannot place.\n" +
			"It can never reach the exchange, but it rests as if it were working: risk carries exposure for it, " +
			"the caller was told it was accepted, and for a stop nobody learns otherwise until it fails to fire.")
	}

	oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc == nil || oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("command outcome = %v, want REJECTED — the caller must be TOLD the venue cannot place this type", oc.GetStatus())
	}
	if oc.GetErrorCode() != "ORDER_TYPE_NOT_SUPPORTED" {
		t.Errorf("rejection code = %q, want ORDER_TYPE_NOT_SUPPORTED", oc.GetErrorCode())
	}
	for _, ev := range fb.types() {
		if ev == EventTypeRouted || ev == EventTypeFilled {
			t.Fatalf("the order was %s despite a type the venue cannot place", ev)
		}
	}
}

// A type the venue DOES declare is unaffected. Without this the test above is
// satisfied by an OMS that refuses everything, which would be a trading outage
// wearing the shape of a fix.
func TestDeclaredOrderType_IsStillAdmitted(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	spot := execution.WithOrderTypes(execution.NewSimVenue("XBIN"), []orderpb.OrderType{
		orderpb.OrderType_ORDER_TYPE_MARKET,
		orderpb.OrderType_ORDER_TYPE_LIMIT,
	})
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter(spot), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testCtx()

	cmd := limitOrder(d(100, 0), d(1025, -2)) // LIMIT — declared
	cmd.Venue = "XBIN"
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, _, lerr := store.Load(ctx, cmd.GetOrderId()); lerr != nil {
		t.Fatalf("a LIMIT order was refused by a venue that declares LIMIT: %v", lerr)
	}
	if oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome); oc != nil &&
		oc.GetErrorCode() == "ORDER_TYPE_NOT_SUPPORTED" {
		t.Fatal("a declared order type was refused as unsupported")
	}
}

// AN ADAPTER THAT DECLARED NOTHING KEEPS ITS ORDERS. Empty means "did not say",
// never "supports nothing" — an adapter predating venue.v1's supported_order_types
// must not have every order refused, or a schema addition becomes a trading
// outage. The OMS names and counts that adapter at startup instead, and
// OMS_REQUIRE_ORDER_TYPE_SUPPORT is what closes the gap deliberately.
func TestUndeclaredVenue_AdmitsEveryType(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	// Not wrapped: this is what an old adapter looks like after WithOrderTypes.
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter(execution.WithOrderTypes(execution.NewSimVenue("XBIN"), nil)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testCtx()

	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "XBIN"
	cmd.OrderType = orderpb.OrderType_ORDER_TYPE_STOP_LIMIT
	cmd.StopPrice = d(1000, -2)
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome); oc != nil &&
		oc.GetErrorCode() == "ORDER_TYPE_NOT_SUPPORTED" {
		t.Fatal("an adapter that declared NO order types had an order refused as unsupported.\n" +
			"Empty is \"did not say\", not \"supports nothing\": this turns every un-upgraded adapter into an outage.")
	}
}
