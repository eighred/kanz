package binance

import (
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
)

// The OMS now REFUSES AT ADMISSION an order type this connector does not declare
// (#405), which makes OrderTypes() a load-bearing statement rather than
// documentation: over-declare and the old defect returns — an order admitted,
// stored and announced that the exchange never sees; under-declare and a type
// this connector can place is refused at the gateway and looks like an outage.
//
// Nothing in the type system ties the declaration to the switch in orderParams
// that actually decides. This test does, in BOTH directions, over every value of
// order.v1.OrderType — including ones added to the enum after it was written,
// because it walks the descriptor rather than a list a human keeps in step.
func TestDeclaredOrderTypesMatchTranslation(t *testing.T) {
	v := &BinanceVenue{}
	declared := v.OrderTypes()
	if len(declared) == 0 {
		t.Fatal("OrderTypes() is empty — an empty declaration means \"did not say\" to the OMS, " +
			"which would silently disable the admission gate this connector relies on")
	}

	values := orderpb.OrderType(0).Descriptor().Values()
	checked := 0
	for i := 0; i < values.Len(); i++ {
		ot := orderpb.OrderType(values.Get(i).Number())
		if ot == orderpb.OrderType_ORDER_TYPE_UNSPECIFIED {
			continue // never valid on the wire; the OMS rejects it before routing
		}
		checked++

		// A well-formed order of this type and nothing else wrong with it, so the
		// only thing translation can object to is the type itself.
		//
		// EVERY FIELD ANY TYPE NEEDS IS SET, including the ones a given type
		// ignores. The point is to isolate the TYPE as the only possible objection
		// — a probe missing stop_price would make a correctly-implemented STOP
		// look undeclarable, which is the failure this test had when
		// OrderState.stop_price landed (#442) and the probe was not updated with
		// it.
		st := &orderpb.OrderState{
			OrderId:         "o1",
			InstrumentId:    "BTC-USD",
			Side:            orderpb.Side_SIDE_BUY,
			OrderType:       ot,
			OrderedQuantity: dec.ToProto(dec.Rat("1")),
			LimitPrice:      dec.ToProto(dec.Rat("30000")),
			StopPrice:       dec.ToProto(dec.Rat("29000")),
			TimeInForce:     orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		}
		_, err := orderParams(st, "BTCUSDT")

		want := execution.ContainsOrderType(declared, ot)
		switch {
		case want && err != nil:
			t.Errorf("OrderTypes() declares %v but orderParams refuses it: %v\n"+
				"the OMS will admit this type and the exchange will never see it — "+
				"either implement it in the switch or drop it from the declaration", ot, err)
		case !want && err == nil:
			t.Errorf("orderParams translates %v but OrderTypes() does not declare it\n"+
				"the OMS will refuse it at admission though this connector can place it — "+
				"add it to the declaration", ot)
		}
	}

	// Non-vacuity: a descriptor that yielded nothing would pass every assertion
	// above by making none of them.
	if checked < 4 {
		t.Fatalf("walked %d order types, want >= 4 — order.v1.OrderType declares "+
			"MARKET, LIMIT, STOP and STOP_LIMIT; this test proved nothing", checked)
	}
}

// The declaration must reach the OMS as itself. A connector that satisfies the
// interface but is not seen through it declares nothing on the wire, and "did not
// say" is the case that leaves the admission gate open.
var _ execution.OrderTypeDeclarer = (*BinanceVenue)(nil)

var _ protoreflect.Enum = orderpb.OrderType(0)

// ===== WHAT ACTUALLY REACHES THE EXCHANGE (#405) =====
//
// The declaration test above proves the connector CLAIMS what it can translate.
// These prove the translation is the right one — that a stop carries its trigger,
// and that a time-in-force is not silently replaced.

func probe(ot orderpb.OrderType, tif orderpb.TimeInForce) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId:         "o1",
		InstrumentId:    "BTC-USD",
		Side:            orderpb.Side_SIDE_SELL,
		OrderType:       ot,
		OrderedQuantity: dec.ToProto(dec.Rat("1.5")),
		LimitPrice:      dec.ToProto(dec.Rat("30000")),
		StopPrice:       dec.ToProto(dec.Rat("29000")),
		TimeInForce:     tif,
	}
}

// A STOP CARRIES ITS TRIGGER. Without stopPrice the exchange has no level at
// which to fire, and #405's whole point is that a stop with no trigger is a risk
// control that does nothing — the worst possible shape, because a stop exists to
// act when nobody is watching.
func TestOrderParams_StopCarriesItsTrigger(t *testing.T) {
	p, err := orderParams(probe(orderpb.OrderType_ORDER_TYPE_STOP,
		orderpb.TimeInForce_TIME_IN_FORCE_GTC), "BTCUSDT")
	if err != nil {
		t.Fatalf("orderParams: %v", err)
	}
	if got := p.Get("type"); got != "STOP_LOSS" {
		t.Errorf("type = %q, want STOP_LOSS (a stop becomes a MARKET order on trigger)", got)
	}
	if got := p.Get("stopPrice"); got != "29000" {
		t.Fatalf("stopPrice = %q, want 29000 — a stop with no trigger never fires, and nothing "+
			"downstream would say so until the day it was needed", got)
	}
	// BINANCE REJECTS price AND timeInForce ON STOP_LOSS. Sending them would make
	// every stop fail at the exchange, which is the defect #405 exists to end.
	if got := p.Get("price"); got != "" {
		t.Errorf("price = %q on a STOP_LOSS; Binance rejects the parameter on this type", got)
	}
	if got := p.Get("timeInForce"); got != "" {
		t.Errorf("timeInForce = %q on a STOP_LOSS; Binance rejects the parameter on this type", got)
	}
}

// A STOP-LIMIT CARRIES BOTH PRICES. The trigger and the limit are different
// numbers doing different jobs, and sending one for the other would fire at the
// wrong level or fill at the wrong price.
func TestOrderParams_StopLimitCarriesBothPrices(t *testing.T) {
	p, err := orderParams(probe(orderpb.OrderType_ORDER_TYPE_STOP_LIMIT,
		orderpb.TimeInForce_TIME_IN_FORCE_GTC), "BTCUSDT")
	if err != nil {
		t.Fatalf("orderParams: %v", err)
	}
	if got := p.Get("type"); got != "STOP_LOSS_LIMIT" {
		t.Errorf("type = %q, want STOP_LOSS_LIMIT", got)
	}
	if p.Get("stopPrice") != "29000" || p.Get("price") != "30000" {
		t.Fatalf("stopPrice=%q price=%q, want 29000 and 30000 — the trigger and the limit are "+
			"different numbers and swapping them fires at the wrong level",
			p.Get("stopPrice"), p.Get("price"))
	}
	if got := p.Get("timeInForce"); got != "GTC" {
		t.Errorf("timeInForce = %q, want GTC — Binance requires it on this type", got)
	}
}

// A STOP WITH NO TRIGGER IS REFUSED, not sent with a zero. Binance would read a
// stopPrice of 0 as "trigger immediately", turning a protective order into an
// instant market order at whatever the book offers.
func TestOrderParams_AStopWithNoTriggerIsRefused(t *testing.T) {
	for _, ot := range []orderpb.OrderType{
		orderpb.OrderType_ORDER_TYPE_STOP,
		orderpb.OrderType_ORDER_TYPE_STOP_LIMIT,
	} {
		st := probe(ot, orderpb.TimeInForce_TIME_IN_FORCE_GTC)
		st.StopPrice = nil
		if _, err := orderParams(st, "BTCUSDT"); err == nil {
			t.Errorf("%v with no stop price was translated — a zero trigger reads as \"fire now\" "+
				"and turns a protective order into an immediate market order", ot)
		}
	}
}

// ===== TIME-IN-FORCE IS NOT SILENTLY REPLACED (#405 family) =====

// THE TRADER'S TIME-IN-FORCE REACHES THE EXCHANGE.
//
// Every LIMIT order this connector ever placed was sent with a hard-coded GTC,
// whatever was asked for. That is worse than the three earlier instances of this
// defect family, because those produced an order that did NOTHING while this one
// produces an order that does the WRONG THING: an IOC sent as GTC RESTS at the
// exchange, so a trader who asked not to hold exposure is holding it — and
// nothing anywhere says so.
func TestOrderParams_TimeInForceReachesTheExchange(t *testing.T) {
	tests := []struct {
		tif  orderpb.TimeInForce
		want string
		harm string
	}{
		{orderpb.TimeInForce_TIME_IN_FORCE_GTC, "GTC", ""},
		{
			orderpb.TimeInForce_TIME_IN_FORCE_IOC, "IOC",
			"an IOC sent as GTC rests at the exchange: the trader asked to hold no exposure " +
				"and is holding it",
		},
		{
			orderpb.TimeInForce_TIME_IN_FORCE_FOK, "FOK",
			"a FOK sent as GTC can rest PARTIALLY FILLED, which is the one outcome that " +
				"instruction exists to forbid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			p, err := orderParams(probe(orderpb.OrderType_ORDER_TYPE_LIMIT, tt.tif), "BTCUSDT")
			if err != nil {
				t.Fatalf("orderParams: %v", err)
			}
			if got := p.Get("timeInForce"); got != tt.want {
				t.Fatalf("timeInForce = %q, want %q — %s", got, tt.want, tt.harm)
			}
		})
	}
}

// A TIME-IN-FORCE BINANCE CANNOT EXPRESS IS REFUSED, not approximated.
//
// Binance Spot has no trading session (so DAY is meaningless) and no
// good-til-date parameter. Sending either as GTC would rest an order the trader
// asked to expire — the same silent substitution, one value over.
func TestOrderParams_AnInexpressibleTimeInForceIsRefusedNotApproximated(t *testing.T) {
	for _, tif := range []orderpb.TimeInForce{
		orderpb.TimeInForce_TIME_IN_FORCE_DAY,
		orderpb.TimeInForce_TIME_IN_FORCE_GTD,
	} {
		if _, err := orderParams(probe(orderpb.OrderType_ORDER_TYPE_LIMIT, tif), "BTCUSDT"); err == nil {
			t.Errorf("%v was translated — Binance spot cannot express it, and sending GTC would "+
				"rest an order the trader asked to expire", tif)
		}
	}
}

// A MARKET ORDER CARRIES NO TIME-IN-FORCE. Binance rejects the parameter on
// MARKET, so refusing a DAY order here would break market orders that carry a
// time-in-force the OMS happens to have defaulted.
func TestOrderParams_MarketCarriesNoTimeInForce(t *testing.T) {
	p, err := orderParams(probe(orderpb.OrderType_ORDER_TYPE_MARKET,
		orderpb.TimeInForce_TIME_IN_FORCE_DAY), "BTCUSDT")
	if err != nil {
		t.Fatalf("a MARKET order was refused over a time-in-force it does not use: %v", err)
	}
	if got := p.Get("timeInForce"); got != "" {
		t.Errorf("timeInForce = %q on a MARKET order; Binance rejects the parameter", got)
	}
}
