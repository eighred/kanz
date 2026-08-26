package okx

import (
	"context"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution"
)

// MARGIN MODE IS THE FIFTH CAPABILITY, AND IT SHIPPED WITHOUT THE GUARDS THE
// OTHER FOUR CARRY (#742).
//
// The family is #240 (leverage), #405 (stop_price and order types), #486
// (time_in_force) and now #417's margin mode: in each, the platform believed a
// term the venue never heard. The mechanism that closes them is always the same
// pair — the connector DECLARES what it can express, and the OMS refuses the
// rest at admission — and the guard that keeps the pair honest is always a drift
// test walking the proto descriptor in both directions.
//
// Order types and time-in-force have that test on both connectors. Margin mode
// had none: `grep MarginMode services/venue-*/**/*_test.go` returned nothing.
// The behaviour was correct; nothing kept it correct.
//
// THIS TEST DOES NOT SKIP UNSPECIFIED, and that is the one place it must differ
// from the two tests it copies. They skip their zero value because it is never
// valid on the wire. MARGIN_MODE_UNSPECIFIED is the opposite: it MEANS spot, it
// is the only value either connector declares, and skipping it would leave this
// walking two undeclared values against a set containing nothing they could
// match — passing while proving nothing at all.

func TestDeclaredMarginModesMatchTranslation(t *testing.T) {
	v := &OKXVenue{}
	declared := v.MarginModes()
	if len(declared) == 0 {
		t.Fatal("MarginModes() is empty — an empty declaration means \"did not say\" to the OMS, " +
			"which leaves the admission gate OPEN for every collateral regime, including ones this " +
			"connector would refuse at the wire")
	}

	values := orderpb.MarginMode(0).Descriptor().Values()
	checked := 0
	for i := 0; i < values.Len(); i++ {
		m := orderpb.MarginMode(values.Get(i).Number())
		checked++

		got := marginModeSupported(m)
		want := execution.ContainsMarginMode(declared, m)
		switch {
		case want && !got:
			t.Errorf("MarginModes() declares %v but Execute refuses it — the OMS will admit an "+
				"order this connector then rejects at the wire, after the estate was told it existed", m)
		case !want && got:
			t.Errorf("Execute accepts %v but MarginModes() does not declare it — an order under "+
				"that regime is refused at admission though this connector would place it, and the "+
				"gap reads to an operator as an outage", m)
		}
	}

	if checked < 3 {
		t.Fatalf("walked %d margin mode(s), want >= 3 — order.v1 declares UNSPECIFIED, CROSS and "+
			"ISOLATED; this test proved nothing", checked)
	}
}

// THE REFUSAL MUST HAPPEN BEFORE THE EXCHANGE IS TOUCHED.
//
// Asserting only that an error came back would pass for a connector that placed
// the order and then failed — which is the exact defect this refusal exists to
// prevent: a live spot position while the platform's audit root records margin.
// So the assertion is that NO request reached the venue.
func TestExecuteRefusesANonSpotRegimeWithoutTouchingTheExchange(t *testing.T) {
	for _, m := range []orderpb.MarginMode{
		orderpb.MarginMode_MARGIN_MODE_CROSS,
		orderpb.MarginMode_MARGIN_MODE_ISOLATED,
	} {
		t.Run(m.String(), func(t *testing.T) {
			f := newFakeOKX(t)
			// A body that WOULD succeed, so a connector that ignored the regime
			// gets a clean placement rather than an error for another reason.
			f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"m1","sCode":"0","sMsg":""}]}`
			f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"m1","state":"filled","accFillSz":"1","avgPx":"50000"}]}`
			v := okxVenueOver(f)

			st := okxMarket("m1")
			st.MarginMode = m

			fills, err := v.Execute(context.Background(), st)
			if err == nil {
				t.Fatalf("Execute placed an order under %v — this connector sends tdMode cash, so "+
					"the position would be unlevered while the audit root recorded margin", m)
			}
			if f.posts != 0 {
				t.Fatalf("posts = %d, want 0 — the order reached OKX before being refused, which is "+
					"the whole failure: the refusal is only worth anything if nothing was placed", f.posts)
			}
			if len(fills) != 0 {
				t.Fatalf("fills = %d, want 0 on a refusal", len(fills))
			}
		})
	}
}

// NON-VACUITY for the refusal above: the SAME order, under the regime this
// connector does declare, must still be placed. Without this row a connector
// that refused everything would satisfy every assertion above.
func TestExecuteStillPlacesASpotOrder(t *testing.T) {
	f := newFakeOKX(t)
	f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"m1","sCode":"0","sMsg":""}]}`
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"m1","state":"filled","accFillSz":"1","avgPx":"50000"}]}`
	v := okxVenueOver(f)

	st := okxMarket("m1")
	st.MarginMode = orderpb.MarginMode_MARGIN_MODE_UNSPECIFIED

	if _, err := v.Execute(context.Background(), st); err != nil {
		t.Fatalf("Execute refused a SPOT order: %v — UNSPECIFIED is spot, which is exactly what "+
			"this connector places", err)
	}
	if f.posts != 1 {
		t.Fatalf("posts = %d, want 1 — the spot order must actually reach the exchange", f.posts)
	}
}
