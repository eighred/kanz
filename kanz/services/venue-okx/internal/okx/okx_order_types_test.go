package okx

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
