package okx

import (
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/orderid"
	"strings"
)

// The OMS now REFUSES AT ADMISSION an order type this connector does not declare
// (#405), which makes OrderTypes() a load-bearing statement rather than
// documentation: over-declare and the old defect returns — an order admitted,
// stored and announced that the exchange never sees; under-declare and a type
// this connector can place is refused at the gateway and looks like an outage.
//
// Nothing in the type system ties the declaration to the builders that actually
// decide — okxOrderBody for regular orders, okxAlgoBody for conditional ones. This test does, in BOTH directions, over every value of
// order.v1.OrderType — including ones added to the enum after it was written,
// because it walks the descriptor rather than a list a human keeps in step.
func TestDeclaredOrderTypesMatchTranslation(t *testing.T) {
	v := &OKXVenue{}
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
		st := &orderpb.OrderState{
			OrderId:         "o1",
			InstrumentId:    "BTC-USD",
			Side:            orderpb.Side_SIDE_BUY,
			OrderType:       ot,
			OrderedQuantity: dec.ToProto(dec.Rat("1")),
			LimitPrice:      dec.ToProto(dec.Rat("30000")),
			// EVERY FIELD ANY TYPE NEEDS, including ones a given type ignores. A
			// probe missing stop_price would make a correctly implemented STOP look
			// undeclarable — exactly how the Binance copy of this guard went stale
			// when #442 added the field and the fixture was not updated with it.
			StopPrice:   dec.ToProto(dec.Rat("29000")),
			TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_GTC,
		}

		// TWO BUILDERS, ONE QUESTION. A stop is a CONDITIONAL order at OKX and
		// goes through okxAlgoBody on a different endpoint (#485); everything else
		// goes through okxOrderBody. What this guard asks is whether the connector
		// can translate the type AT ALL, so it has to ask the builder that type
		// would actually use — consulting only one would report every stop as
		// undeclarable the moment stops started working.
		var err error
		if isAlgoOrder(ot) {
			_, err = okxAlgoBody(st, "BTC-USDT")
		} else {
			_, err = okxOrderBody(st, "BTC-USDT")
		}

		want := execution.ContainsOrderType(declared, ot)
		switch {
		case want && err != nil:
			t.Errorf("OrderTypes() declares %v but okxOrderBody refuses it: %v\n"+
				"the OMS will admit this type and the exchange will never see it — "+
				"either implement it or drop it from the declaration", ot, err)
		case !want && err == nil:
			t.Errorf("okxOrderBody translates %v but OrderTypes() does not declare it\n"+
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
var _ execution.OrderTypeDeclarer = (*OKXVenue)(nil)

var _ protoreflect.Enum = orderpb.OrderType(0)

// ===== TIME-IN-FORCE IS NOT SILENTLY REPLACED (#405 family) =====
//
// OKX carries time-in-force in ordType itself. This connector sent "limit" for
// every priced order whatever the trader asked for — which is worse than the
// three earlier instances of this defect family, because those produced an order
// that did NOTHING while this one produces an order that does the WRONG THING.

func tifProbe(tif orderpb.TimeInForce) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId:         "o1",
		InstrumentId:    "BTC-USD",
		Side:            orderpb.Side_SIDE_SELL,
		OrderType:       orderpb.OrderType_ORDER_TYPE_LIMIT,
		OrderedQuantity: dec.ToProto(dec.Rat("1.5")),
		LimitPrice:      dec.ToProto(dec.Rat("30000")),
		TimeInForce:     tif,
	}
}

func TestOKXOrderBody_TimeInForceReachesTheExchange(t *testing.T) {
	tests := []struct {
		tif  orderpb.TimeInForce
		want string
		harm string
	}{
		{orderpb.TimeInForce_TIME_IN_FORCE_GTC, "limit", ""},
		{
			orderpb.TimeInForce_TIME_IN_FORCE_IOC, "ioc",
			"an IOC sent as a resting limit stays at the exchange: the trader asked to hold no " +
				"exposure and is holding it",
		},
		{
			orderpb.TimeInForce_TIME_IN_FORCE_FOK, "fok",
			"a FOK sent as a resting limit can rest PARTIALLY FILLED, which is the one outcome " +
				"that instruction exists to forbid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			body, err := okxOrderBody(tifProbe(tt.tif), "BTC-USDT")
			if err != nil {
				t.Fatalf("okxOrderBody: %v", err)
			}
			if got := body["ordType"]; got != tt.want {
				t.Fatalf("ordType = %q, want %q — %s", got, tt.want, tt.harm)
			}
			if body["px"] != "30000" {
				t.Errorf("px = %q, want the limit price to survive the time-in-force mapping",
					body["px"])
			}
		})
	}
}

// A TIME-IN-FORCE OKX CANNOT EXPRESS IS REFUSED, not approximated.
func TestOKXOrderBody_AnInexpressibleTimeInForceIsRefused(t *testing.T) {
	for _, tif := range []orderpb.TimeInForce{
		orderpb.TimeInForce_TIME_IN_FORCE_DAY,
		orderpb.TimeInForce_TIME_IN_FORCE_GTD,
	} {
		if _, err := okxOrderBody(tifProbe(tif), "BTC-USDT"); err == nil {
			t.Errorf("%v was translated — OKX spot cannot express it, and a resting limit keeps "+
				"an order the trader asked to expire", tif)
		}
	}
}

// A MARKET ORDER IS UNAFFECTED. OKX market orders are inherently immediate, so a
// time-in-force it does not use must not refuse the order.
func TestOKXOrderBody_MarketIsUnaffectedByTimeInForce(t *testing.T) {
	st := tifProbe(orderpb.TimeInForce_TIME_IN_FORCE_DAY)
	st.OrderType = orderpb.OrderType_ORDER_TYPE_MARKET
	body, err := okxOrderBody(st, "BTC-USDT")
	if err != nil {
		t.Fatalf("a MARKET order was refused over a time-in-force it does not use: %v", err)
	}
	if got := body["ordType"]; got != "market" {
		t.Errorf("ordType = %q, want market", got)
	}
}

// ===== AN ORDER ID OKX CANNOT ACCEPT IS REFUSED WITH THE REASON =====
//
// Established by probing OKX's demo API on 2026-08-15: it accepts letters and
// digits only, at most 32 characters, and answers anything else with
// `51000 Parameter clOrdId error` — which does not say which of a 36-character
// UUID's characters was the problem.
//
// api-gateway minted order ids with uuid.NewString(), so EVERY order submitted
// through the HTTP gateway was unplaceable here: admitted, stored, its
// ORDER_ACCEPTED FACT published, then refused by the exchange. Binance accepts
// hyphens and 36 characters, so the same order traded normally there — which is
// why nothing noticed.
func TestOKXOrderBody_RefusesAnOrderIDOKXCannotAccept(t *testing.T) {
	for _, tt := range []struct{ name, id string }{
		{"a hyphenated UUID, as api-gateway used to mint", "3f9a1c2e-0b7d-4e11-9a6f-2c8d5e4b7a13"},
		{"33 characters", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"an underscore", "kanz_order_1"},
		{"a dot", "kanz.order.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := tifProbe(orderpb.TimeInForce_TIME_IN_FORCE_GTC)
			st.OrderId = tt.id
			_, err := okxOrderBody(st, "BTC-USDT")
			if err == nil {
				t.Fatalf("order id %q was sent to OKX — it comes back as \"Parameter clOrdId "+
					"error\", which names neither the rule nor the character", tt.id)
			}
			if !strings.Contains(err.Error(), "clOrdId") {
				t.Errorf("error = %q, want it to name the venue's field so an operator knows "+
					"what to change", err)
			}
		})
	}
}

// AND THE IDS THE ESTATE ACTUALLY MINTS ARE ACCEPTED. A rule that refused
// everything would be a trading outage wearing a control's shape.
func TestOKXOrderBody_AcceptsTheIdsThisEstateMints(t *testing.T) {
	for _, id := range []string{
		"8d1f0c3b9a2e4d5f6071829304a5b6c7", // signal fan-out, and now the gateway
		orderid.Mint(),                     // freshly minted
	} {
		st := tifProbe(orderpb.TimeInForce_TIME_IN_FORCE_GTC)
		st.OrderId = id
		if _, err := okxOrderBody(st, "BTC-USDT"); err != nil {
			t.Errorf("order id %q was refused: %v", id, err)
		}
	}
}

// ===== CONDITIONAL (STOP) ORDERS (#485) =====
//
// Every claim below was measured against OKX's demo API on 2026-08-15, including
// firing a real trigger and reading back the order it created.

func stopProbe(ot orderpb.OrderType) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId:         "8d1f0c3b9a2e4d5f6071829304a5b6c7",
		InstrumentId:    "BTC-USD",
		Side:            orderpb.Side_SIDE_SELL,
		OrderType:       ot,
		OrderedQuantity: dec.ToProto(dec.Rat("0.5")),
		LimitPrice:      dec.ToProto(dec.Rat("30000")),
		StopPrice:       dec.ToProto(dec.Rat("29000")),
		TimeInForce:     orderpb.TimeInForce_TIME_IN_FORCE_GTC,
	}
}

// A STOP CARRIES OUR ID ON algoClOrdId, AND THAT IS THE FIELD THAT SURVIVES THE
// TRIGGER.
//
// The algo order is not the order that fills — it creates one when it fires, and
// OKX gives that order a clOrdId of its own (observed: "O3833848819892766720").
// Ours rides on algoClOrdId, which the triggered order carries and which
// okx_userdata.go reads first. Put our id in the wrong field and a real
// execution arrives keyed by something this platform has never seen.
func TestOKXAlgoBody_CarriesOurIDWhereTheTriggerPreservesIt(t *testing.T) {
	body, err := okxAlgoBody(stopProbe(orderpb.OrderType_ORDER_TYPE_STOP), "BTC-USDT")
	if err != nil {
		t.Fatalf("okxAlgoBody: %v", err)
	}
	if got := body["algoClOrdId"]; got != "8d1f0c3b9a2e4d5f6071829304a5b6c7" {
		t.Fatalf("algoClOrdId = %q, want our order id — it is the only field that reaches the "+
			"order the trigger creates, so the fill would be unattributable", got)
	}
	if _, ok := body["clOrdId"]; ok {
		t.Error("clOrdId was set on an algo order; OKX overwrites it with one of its own and " +
			"the value is silently discarded")
	}
}

// A STOP BECOMES A MARKET ORDER ON TRIGGER; A STOP-LIMIT BECOMES A LIMIT ONE.
// OKX spells the first as orderPx -1, and that single field is the whole
// difference between the two instructions.
func TestOKXAlgoBody_TheTriggeredOrderTypeIsTheWholeDifference(t *testing.T) {
	stop, err := okxAlgoBody(stopProbe(orderpb.OrderType_ORDER_TYPE_STOP), "BTC-USDT")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stop["ordType"] != "trigger" {
		t.Errorf("ordType = %q, want trigger", stop["ordType"])
	}
	if stop["triggerPx"] != "29000" {
		t.Fatalf("triggerPx = %q, want 29000 — a stop with no trigger never fires, and nothing "+
			"says so until the day it was needed", stop["triggerPx"])
	}
	if stop["orderPx"] != "-1" {
		t.Fatalf("orderPx = %q, want -1 — anything else turns a STOP into a stop-LIMIT, which "+
			"can fail to fill in exactly the fast market the stop was placed for", stop["orderPx"])
	}

	lim, err := okxAlgoBody(stopProbe(orderpb.OrderType_ORDER_TYPE_STOP_LIMIT), "BTC-USDT")
	if err != nil {
		t.Fatalf("stop-limit: %v", err)
	}
	if lim["triggerPx"] != "29000" || lim["orderPx"] != "30000" {
		t.Fatalf("triggerPx=%q orderPx=%q, want 29000 and 30000 — the trigger and the limit are "+
			"different numbers and swapping them fires at the wrong level",
			lim["triggerPx"], lim["orderPx"])
	}
}

// A STOP WITH NO TRIGGER, OR A STOP-LIMIT WITH NO LIMIT, IS REFUSED. A zero
// trigger reads as "fire now" and turns a protective order into an immediate one.
func TestOKXAlgoBody_RefusesAnIncompleteStop(t *testing.T) {
	noTrigger := stopProbe(orderpb.OrderType_ORDER_TYPE_STOP)
	noTrigger.StopPrice = nil
	if _, err := okxAlgoBody(noTrigger, "BTC-USDT"); err == nil {
		t.Error("a stop with no trigger price was translated")
	}
	noLimit := stopProbe(orderpb.OrderType_ORDER_TYPE_STOP_LIMIT)
	noLimit.LimitPrice = nil
	if _, err := okxAlgoBody(noLimit, "BTC-USDT"); err == nil {
		t.Error("a stop-limit with no limit price was translated")
	}
}

// AND AN ORDER ID OKX CANNOT ACCEPT IS REFUSED HERE TOO. The algo endpoint
// enforces the same 1-32 alphanumeric rule as the regular one, so the check
// cannot live only on the regular path.
func TestOKXAlgoBody_RefusesAnUnplaceableOrderID(t *testing.T) {
	st := stopProbe(orderpb.OrderType_ORDER_TYPE_STOP)
	st.OrderId = "3f9a1c2e-0b7d-4e11-9a6f-2c8d5e4b7a13"
	if _, err := okxAlgoBody(st, "BTC-USDT"); err == nil {
		t.Error("a hyphenated UUID was sent as algoClOrdId; OKX answers 51000 and the stop " +
			"never exists")
	}
}

// A STOP IS AN ALGO ORDER AND EVERYTHING ELSE IS NOT — the predicate that decides
// which endpoint placement, query and cancel each use. Derived from the order
// type rather than remembered, so a cancel arriving minutes later on another pod
// still reaches the right endpoint.
func TestOKXIsAlgoOrder_SplitsExactlyTheConditionalTypes(t *testing.T) {
	for _, ot := range []orderpb.OrderType{
		orderpb.OrderType_ORDER_TYPE_STOP,
		orderpb.OrderType_ORDER_TYPE_STOP_LIMIT,
	} {
		if !isAlgoOrder(ot) {
			t.Errorf("%v is not treated as an algo order — it would be placed on the regular "+
				"endpoint, which cannot express a trigger", ot)
		}
	}
	for _, ot := range []orderpb.OrderType{
		orderpb.OrderType_ORDER_TYPE_MARKET,
		orderpb.OrderType_ORDER_TYPE_LIMIT,
	} {
		if isAlgoOrder(ot) {
			t.Errorf("%v is treated as an algo order — it would be placed as a conditional "+
				"order that never fires", ot)
		}
	}
}
