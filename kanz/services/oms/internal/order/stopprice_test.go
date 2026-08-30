package order

import (
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

// THE TRIGGER PRICE MUST REACH THE WIRE, NOT ONLY THE VALIDATOR (#405).
//
// Admission has always REQUIRED stop_price on a stop order — "stop_price
// required for a stop order" — and OrderState had no field to carry it, so the
// value was checked at the perimeter and then dropped. OrderState is what
// Venue.Execute receives, so a connector implementing Binance STOP_LOSS_LIMIT or
// OKX's conditional-order endpoint had NOTHING to send as the trigger. It could
// place an order that would never fire, at whatever level the venue defaulted
// to — for the one order type whose entire purpose is to act when nobody is
// watching.
//
// CLOSED #240 IS THE SAME DEFECT ONE FIELD OVER: `leverage` was parsed,
// bounds-checked, written to the immutable FACT, and then dropped before the
// order, so the audit root asserted 10x on a spot position. Two instances make a
// class, which is why test/arch/submit_fields_reach_state_test.go now fails the
// build for a third.

func stopLimitOrder(qty, limit, stop *commonpb.Decimal) *orderpb.SubmitOrder {
	return &orderpb.SubmitOrder{
		OrderId:      "o-stop",
		PortfolioId:  "pf1",
		InstrumentId: "AAPL",
		Side:         orderpb.Side_SIDE_SELL, // a protective stop is the motivating case
		Quantity:     qty,
		OrderType:    orderpb.OrderType_ORDER_TYPE_STOP_LIMIT,
		LimitPrice:   limit,
		StopPrice:    stop,
		TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_DAY,
	}
}

func TestAccept_CarriesTheStopPriceOntoTheState(t *testing.T) {
	stop := d(9500, -2) // 95.00 — the level the trader chose
	st, err := Accept(stopLimitOrder(d(10, 0), d(9450, -2), stop), t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if st.GetStopPrice() == nil {
		t.Fatal("OrderState.stop_price is nil after admission.\n" +
			"Admission REQUIRED this value and then discarded it, so the OrderState that " +
			"reaches Venue.Execute cannot say at what level the order should trigger. A " +
			"connector has nothing to send, and the trader's stop never fires.")
	}
	if dec.Cmp(st.GetStopPrice(), stop) != 0 {
		t.Fatalf("stop_price = %v, want %v — the trigger must be carried EXACTLY. "+
			"A stop that fires at a different level than the trader set is worse than one "+
			"that does not fire, because it looks like it worked.", st.GetStopPrice(), stop)
	}
}

// A PLAIN STOP CARRIES IT TOO. STOP and STOP_LIMIT differ in what happens AFTER
// the trigger (market vs limit), not in whether there is one — and admission
// requires the field for both.
func TestAccept_CarriesTheStopPriceOnAPlainStop(t *testing.T) {
	stop := d(8800, -2)
	cmd := stopLimitOrder(d(10, 0), nil, stop)
	cmd.OrderType = orderpb.OrderType_ORDER_TYPE_STOP
	cmd.LimitPrice = nil // a plain stop has no limit

	st, err := Accept(cmd, t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if dec.Cmp(st.GetStopPrice(), stop) != 0 {
		t.Fatalf("stop_price = %v, want %v", st.GetStopPrice(), stop)
	}
}

// AN UNTRIGGERED ORDER TYPE CARRIES NO TRIGGER. Without this, "carry it" is
// satisfied by copying the field unconditionally, which would put a stop price
// on every market order — a value downstream readers would have to know to
// ignore, and the kind of thing a connector eventually stops ignoring.
func TestAccept_LeavesTheStopPriceUnsetOnAnUntriggeredOrder(t *testing.T) {
	st, err := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if st.GetStopPrice() != nil {
		t.Fatalf("a LIMIT order carries stop_price = %v, want unset", st.GetStopPrice())
	}
}

// THE TRIGGER SURVIVES THE LIFECYCLE, and this is the half that actually reaches
// the venue. Route() is called between admission and Venue.Execute: a state
// transition that dropped the field would restore the original bug one step
// later, with admission's own test still green.
func TestRoute_PreservesTheStopPrice(t *testing.T) {
	stop := d(9500, -2)
	st, err := Accept(stopLimitOrder(d(10, 0), d(9450, -2), stop), t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	routed, _ := Route(st, t0)
	if dec.Cmp(routed.GetStopPrice(), stop) != 0 {
		t.Fatalf("after Route, stop_price = %v, want %v. Route is on the path to "+
			"Venue.Execute, so a transition that drops the trigger drops it exactly where "+
			"the connector reads it.", routed.GetStopPrice(), stop)
	}
}
