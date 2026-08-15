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
// Nothing in the type system ties the declaration to the switch in okxOrderBody
// that actually decides. This test does, in BOTH directions, over every value of
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
			// NB: order.v1.OrderState carries no stop/trigger price at all, so a
			// STOP could not express itself even if a connector translated it.
			// That schema gap is part of #405's connector phase.
		}
		_, err := okxOrderBody(st, "BTC-USDT")

		want := execution.ContainsOrderType(declared, ot)
		switch {
		case want && err != nil:
			t.Errorf("OrderTypes() declares %v but okxOrderBody refuses it: %v\n"+
				"the OMS will admit this type and the exchange will never see it — "+
				"either implement it in the switch or drop it from the declaration", ot, err)
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
