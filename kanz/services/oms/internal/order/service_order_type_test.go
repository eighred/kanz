package order

import (
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"

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
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{spot}), nil, nil)
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
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{spot}), nil, nil)
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
		execution.NewRouter([]execution.Venue{execution.WithOrderTypes(execution.NewSimVenue("XBIN"), nil)}), nil, nil)
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

// A TIME-IN-FORCE THE VENUE CANNOT EXPRESS IS REJECTED AT ADMISSION (#486) —
// the same gate as above, one field over, for a defect that was worse.
//
// The order-type cases produced an order that did NOTHING: accepted, inert,
// never placed. time_in_force produced an order that did the WRONG THING. Both
// spot connectors sent a hard-coded good-til-cancelled whatever the trader asked
// for, so an IMMEDIATE-OR-CANCEL order RESTED at the exchange — a trader who
// asked to hold no exposure was holding it, indefinitely, and nothing anywhere
// said so.
//
// The connectors now refuse what they cannot express, which stopped the wrong
// TRADE. This stops the wrong ADMISSION: without it the order is stored, its
// ORDER_ACCEPTED FACT committed and published, and only then refused by the
// connector — which is #405's complaint verbatim.
func TestTimeInForceTheVenueCannotExpress_IsRejected(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()

	// A venue declaring exactly what both live spot adapters declare: no trading
	// session, so no DAY, and no good-til-date parameter, so no GTD.
	spot := execution.WithTimeInForce(
		execution.WithOrderTypes(execution.NewSimVenue("XBIN"), []orderpb.OrderType{
			orderpb.OrderType_ORDER_TYPE_MARKET,
			orderpb.OrderType_ORDER_TYPE_LIMIT,
		}),
		[]orderpb.TimeInForce{
			orderpb.TimeInForce_TIME_IN_FORCE_GTC,
			orderpb.TimeInForce_TIME_IN_FORCE_IOC,
			orderpb.TimeInForce_TIME_IN_FORCE_FOK,
		})
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{spot}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testCtx()

	// A WELL-FORMED limit order whose ONLY problem is its time-in-force. GTD also
	// requires expire_at, or Accept refuses it first and this test would pass
	// without the gate ever running.
	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "XBIN"
	cmd.TimeInForce = orderpb.TimeInForce_TIME_IN_FORCE_GTD
	cmd.ExpireAt = timestamppb.New(t0.Add(24 * time.Hour))
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if _, _, lerr := store.Load(ctx, cmd.GetOrderId()); lerr == nil {
		t.Fatal("the order was ADMITTED with a time-in-force its venue cannot express.\n" +
			"It is stored and announced, and the connector refuses it afterwards — so the caller " +
			"was told the order exists, risk carries exposure for it, and only the exchange " +
			"disagrees.")
	}

	oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc == nil || oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("command outcome = %v, want REJECTED — the caller must be TOLD", oc.GetStatus())
	}
	if oc.GetErrorCode() != "TIME_IN_FORCE_NOT_SUPPORTED" {
		t.Errorf("rejection code = %q, want TIME_IN_FORCE_NOT_SUPPORTED — an operator reading "+
			"ORDER_TYPE_NOT_SUPPORTED for a time-in-force problem would change the wrong field",
			oc.GetErrorCode())
	}
}

// A DECLARED TIME-IN-FORCE IS STILL ADMITTED. Without this the test above is
// satisfied by an OMS that refuses everything, which is a trading outage wearing
// the shape of a fix.
func TestDeclaredTimeInForce_IsStillAdmitted(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	spot := execution.WithTimeInForce(execution.NewSimVenue("XBIN"), []orderpb.TimeInForce{
		orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		orderpb.TimeInForce_TIME_IN_FORCE_IOC,
	})
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{spot}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testCtx()

	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "XBIN"
	cmd.TimeInForce = orderpb.TimeInForce_TIME_IN_FORCE_IOC
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, _, lerr := store.Load(ctx, cmd.GetOrderId()); lerr != nil {
		t.Fatalf("an IOC the venue DECLARED was refused: %v", lerr)
	}
}

// AN UNDECLARING VENUE STILL TRADES. "Did not say" is not "supports nothing",
// and refusing every order for an adapter that predates the field would turn a
// schema addition into a trading outage.
func TestAnUndeclaringVenue_StillAdmitsEveryTimeInForce(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{execution.NewSimVenue("XBIN")}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testCtx()

	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "XBIN"
	cmd.TimeInForce = orderpb.TimeInForce_TIME_IN_FORCE_GTD
	cmd.ExpireAt = timestamppb.New(t0.Add(24 * time.Hour))
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, _, lerr := store.Load(ctx, cmd.GetOrderId()); lerr != nil {
		t.Fatalf("an adapter that declared NOTHING had its order refused: %v — a schema addition "+
			"must not become a trading outage for every adapter that predates it", lerr)
	}
}
