package order

import (
	"context"
	"testing"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/platform/halt"
)

// AN AMEND MUST NOT REWRITE TERMS A VENUE IS STILL WORKING (#740).
//
// handleAmend applied the new size or price and answered EXECUTED while the
// exchange went on working the original order, because nothing in this module
// can send an amend to a venue: venue.v1 has four RPCs and none is amend or
// replace, no Amender/Replacer exists in internal/execution, and neither
// connector wires an amend endpoint. order.proto meanwhile claimed the opposite
// in writing ("an amend resets the order's working priority at the venue").
//
// The cancel path one function away does the opposite for exactly this reason —
// closeAtVenue withdraws AT the venue before recording, so the ledger cannot
// call an order cancelled while it is still fillable. An amend with no venue leg
// is that same lie told about size and price, and no reconciler detects it: they
// compare filled quantity and status, and copy the platform's amended figures
// forward as venue truth.
//
// These tests assert the refusal at the level that matters — THE STORED STATE.
// A handler returns nil for a refusal just as it does for a success (it acks and
// publishes REJECTED), so checking the error would read a refusal as an amend
// that worked. Every case below re-Loads the order and asserts the terms did not
// move.

// amendCmd is an amend from the order's own entitled owner. Entitlement is
// deliberately satisfied: the refusal under test must not be reachable via the
// entitlement gate that already refuses on a different axis.
func amendCmd(qty *commonpb.Decimal) *orderpb.AmendOrder {
	return &orderpb.AmendOrder{
		OrderId:     "o1",
		NewQuantity: qty,
		Metadata: &commandpb.CommandMetadata{
			Issuer: "user:owner", TargetId: "o1", PrincipalPortfolios: []string{"pf1"},
		},
	}
}

// pendingOrderSvc returns a service whose submitted order is PENDING_NEW: no
// router, so nothing sends it anywhere and no venue holds it.
func pendingOrderSvc(t *testing.T, fb *fakeBus) *Service {
	t.Helper()
	svc, err := NewService(testTenant, NewMemoryStore(), NewEmitter(fb), nil, nil, nil, nil,
		WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("submit: %v", err)
	}
	st, _, lerr := svc.store.Load(context.Background(), "o1")
	if lerr != nil {
		t.Fatalf("load: %v", lerr)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW {
		t.Fatalf("status = %v, want PENDING_NEW — this helper must produce an order NO venue holds, "+
			"or the positive case below proves nothing", st.GetStatus())
	}
	return svc
}

// loadOrder re-reads the order so an assertion is made against what was stored,
// never against what the handler returned.
func loadOrder(t *testing.T, svc *Service) *orderpb.OrderState {
	t.Helper()
	st, _, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return st
}

// A ROUTED order is resting at the exchange. Amending it moved the platform's
// record and nothing else.
func TestAmend_RefusedWhileAVenueIsWorkingTheOrder(t *testing.T) {
	fb := &fakeBus{}
	svc, _ := restingOrderOn(t, fb, &closerVenue{mic: "BINANCE"})

	if err := svc.Handle(testCtx(), amendEnv(), mustMarshal(t, amendCmd(d(50, 0)))); err != nil {
		t.Fatalf("amend: %v", err)
	}

	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED ||
		oc.GetErrorCode() != ReasonVenueCannotAmend {
		t.Fatalf("outcome = %v/%q, want REJECTED/%s", oc.GetStatus(), oc.GetErrorCode(), ReasonVenueCannotAmend)
	}
	// THE ASSERTION THAT MATTERS. A REJECTED outcome over a mutated store is the
	// original defect wearing a refusal's label: the venue works 100, and the
	// platform would book 50.
	if got := loadOrder(t, svc).GetOrderedQuantity().GetCoefficient(); got != 100 {
		t.Fatalf("ordered quantity = %d, want 100 — the amend was refused and applied anyway", got)
	}
}

// A PARTIALLY_FILLED order is the worst case: the venue is working the
// remainder right now, and a down-amend would book a quantity the exchange is
// actively trading past.
func TestAmend_RefusedOnAPartiallyFilledOrder(t *testing.T) {
	fb := &fakeBus{}
	svc, _ := restingOrderOn(t, fb, &closerVenue{mic: "BINANCE"})

	st, ver, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	st.Status = orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED
	if err := svc.store.Save(context.Background(), st, ver, nil); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := svc.Handle(testCtx(), amendEnv(), mustMarshal(t, amendCmd(d(50, 0)))); err != nil {
		t.Fatalf("amend: %v", err)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetErrorCode() != ReasonVenueCannotAmend {
		t.Fatalf("error code = %q, want %s", oc.GetErrorCode(), ReasonVenueCannotAmend)
	}
	if got := loadOrder(t, svc).GetOrderedQuantity().GetCoefficient(); got != 100 {
		t.Fatalf("ordered quantity = %d, want 100", got)
	}
}

// WORKING_SCHEDULED is the case a "just check whether it is routed" reading
// misses. The parent rests nowhere, but its CHILDREN are at the venue carrying
// the terms an amend claims to change — so a parent amend is the same
// divergence one level up.
func TestAmend_RefusedOnAScheduleParentWhoseChildrenAreAtTheVenue(t *testing.T) {
	fb := &fakeBus{}
	svc, _ := restingOrderOn(t, fb, &closerVenue{mic: "BINANCE"})

	st, ver, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	st.Status = orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED
	if err := svc.store.Save(context.Background(), st, ver, nil); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := svc.Handle(testCtx(), amendEnv(), mustMarshal(t, amendCmd(d(50, 0)))); err != nil {
		t.Fatalf("amend: %v", err)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetErrorCode() != ReasonVenueCannotAmend {
		t.Fatalf("error code = %q, want %s — a schedule parent's slices are AT the venue, and the "+
			"parent's terms are what produced them", oc.GetErrorCode(), ReasonVenueCannotAmend)
	}
	if got := loadOrder(t, svc).GetOrderedQuantity().GetCoefficient(); got != 100 {
		t.Fatalf("ordered quantity = %d, want 100", got)
	}
}

// A PRICE amend is refused on the same terms as a quantity amend. It is the
// quieter of the two failures — the screen shows the new price while the
// exchange works the old one — so it must not fall through a quantity-shaped
// guard.
func TestAmend_RefusedForAPriceChangeToo(t *testing.T) {
	fb := &fakeBus{}
	svc, _ := restingOrderOn(t, fb, &closerVenue{mic: "BINANCE"})

	amend := amendCmd(nil)
	amend.NewLimitPrice = d(900, -2)
	if err := svc.Handle(testCtx(), amendEnv(), mustMarshal(t, amend)); err != nil {
		t.Fatalf("amend: %v", err)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetErrorCode() != ReasonVenueCannotAmend {
		t.Fatalf("error code = %q, want %s", oc.GetErrorCode(), ReasonVenueCannotAmend)
	}
	if got := loadOrder(t, svc).GetLimitPrice().GetCoefficient(); got != 1025 {
		t.Fatalf("limit price = %d, want 1025 — the trader pulled the limit and the exchange never heard", got)
	}
}

// NON-VACUITY. An order NO venue holds is still amendable — the refusal is
// about venue divergence, not about amending being switched off. Without this,
// every test above would pass on a handler that refuses everything.
func TestAmend_StillAppliesBeforeTheOrderReachesAVenue(t *testing.T) {
	fb := &fakeBus{}
	svc := pendingOrderSvc(t, fb)

	if err := svc.Handle(testCtx(), amendEnv(), mustMarshal(t, amendCmd(d(50, 0)))); err != nil {
		t.Fatalf("amend: %v", err)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v/%q, want EXECUTED — a PENDING_NEW order is at no venue, so amending "+
			"it is a pure book-of-record edit and must still work", oc.GetStatus(), oc.GetErrorCode())
	}
	if got := loadOrder(t, svc).GetOrderedQuantity().GetCoefficient(); got != 50 {
		t.Fatalf("ordered quantity = %d, want 50 — the pre-routing amend did not apply", got)
	}
}

// The predicate, at the unit. It is the whole control, so its table is written
// out rather than inferred from the handler tests above.
func TestVenueMayBeWorking_CoversEveryStatus(t *testing.T) {
	want := map[orderpb.OrderStatus]bool{
		orderpb.OrderStatus_ORDER_STATUS_UNSPECIFIED:       false,
		orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW:       false,
		orderpb.OrderStatus_ORDER_STATUS_ROUTED:            true,
		orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED:  true,
		orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED: true,
		// Terminal: never reaches the check — Amend's IsTerminal guard refuses
		// these first, with a better message.
		orderpb.OrderStatus_ORDER_STATUS_FILLED:    false,
		orderpb.OrderStatus_ORDER_STATUS_CANCELLED: false,
		orderpb.OrderStatus_ORDER_STATUS_REJECTED:  false,
		orderpb.OrderStatus_ORDER_STATUS_EXPIRED:   false,
	}
	// EVERY declared status is named. A status added later without a decision
	// here would otherwise default to "amendable" — the fail-open direction.
	for name, num := range orderpb.OrderStatus_value {
		st := orderpb.OrderStatus(num)
		exp, named := want[st]
		if !named {
			t.Fatalf("%s is not in this table: a new order status defaults to AMENDABLE, which is "+
				"the fail-open direction. Decide whether a venue can be working it", name)
		}
		if got := venueMayBeWorking(&orderpb.OrderState{Status: st}); got != exp {
			t.Errorf("venueMayBeWorking(%s) = %v, want %v", name, got, exp)
		}
	}
}
