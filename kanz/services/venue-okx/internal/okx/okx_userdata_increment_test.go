package okx

// THE FILL QUANTITY IS AN INCREMENT, AND NOTHING PROVED IT (#1046).
//
// The OMS order aggregate ADDS fill.quantity to filled_quantity
// (services/oms/internal/order/aggregate.go), so what this ingester publishes
// must be the LAST execution (OKX's fillSz) and never the cumulative
// (accFillSz). Both are on every orders-channel push, one field apart.
//
// Every websocket fixture in this package had fillSz == accFillSz — including
// okx_fill_identity_test.go's explicit quantity assertion — and no fixture
// anywhere fed two sequential partials on one order. So the property held by
// construction and was pinned by nothing: swapping ParseDec(d.FillSz) for
// ParseDec(d.AccFillSz) left ./services/venue-okx/... FULLY GREEN. Measured on
// main before this file existed, not inferred.
//
// The fixture below is the smallest thing that cannot pass under that edit: two
// partials on ONE order where fillSz differs from accFillSz.

import (
	"context"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// TestOKXUserData_TwoSequentialPartialsPublishIncrementsNotCumulatives.
//
// An order for 1 filling in two legs: 0.4 then 0.6, accFillSz 0.4 then 1. The
// two published Fill.quantity values must be the increments and must sum to the
// final cumulative. Under the AccFillSz mutation they are 0.4 and 1, summing to
// 1.4 — forty percent of a position the fund never bought.
//
// Small exact quantities on purpose: odec is dec.ToProto, which WRAPS on large
// values while production parses through ParseDec (#1058), so a fixture built on
// big numbers would be measuring the helper.
func TestOKXUserData_TwoSequentialPartialsPublishIncrementsNotCumulatives(t *testing.T) {
	cap := &okxCapture{}
	frames := [][]byte{
		[]byte(`{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312","clOrdId":"o1","state":"partially_filled","fillSz":"0.4","fillPx":"50000","accFillSz":"0.4","tradeId":"11","fillFee":"-0.02","fillFeeCcy":"USDT","uTime":"1700000001000"}]}`),
		[]byte(`{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312","clOrdId":"o1","state":"filled","fillSz":"0.6","fillPx":"50100","accFillSz":"1","tradeId":"12","fillFee":"-0.03","fillFeeCcy":"USDT","uTime":"1700000002000"}]}`),
	}
	_ = okxIngesterOverView(frames, cap, newOKXOrders("o1")).Run(context.Background())

	var fills []*orderpb.Fill
	var finalState *orderpb.OrderState
	for _, e := range cap.events {
		switch p := e.Payload.(type) {
		case *orderpb.OrderPartiallyFilled:
			fills = append(fills, p.GetFill())
		case *orderpb.OrderFilled:
			fills = append(fills, p.GetFill())
			finalState = p.GetState()
		}
	}
	if len(fills) != 2 {
		t.Fatalf("two fill pushes on one order published %d fill FACTs, want 2", len(fills))
	}

	for i, want := range []string{"0.4", "0.6"} {
		got := dec.FromProto(fills[i].GetQuantity())
		if got.Cmp(dec.Rat(want)) != 0 {
			t.Errorf("fill %d published quantity %s, want %s. The OMS aggregate ADDS this to "+
				"filled_quantity, so publishing accFillSz instead of fillSz double-counts every "+
				"partially filled order into the position book and the ledger", i+1, dec.Str(got), want)
		}
	}

	sum := dec.FromProto(fills[0].GetQuantity())
	sum.Add(sum, dec.FromProto(fills[1].GetQuantity()))
	cum := dec.FromProto(finalState.GetFilledQuantity())
	if sum.Cmp(cum) != 0 {
		t.Errorf("the published increments sum to %s and OKX's final accFillSz is %s. Those must "+
			"agree, or the position the OMS folds up from the fill stream is not the position the "+
			"exchange says it holds", dec.Str(sum), dec.Str(cum))
	}
	if cum.Cmp(dec.Rat("1")) != 0 {
		t.Errorf("the healed state carries cumulative %s, want 1 (accFillSz from the last push)", dec.Str(cum))
	}
}

// TestOKXUserData_AReplayedPushDoesNotRunTheViewBackwards is the end-to-end
// shape of #1046 through the real ingester and the real order view.
//
// The third frame is the FIRST push delivered again — the reconnect replay this
// issue is about. It carries its ORIGINAL uTime, which is how the view tells it
// apart from a fresh one, and it must not move the adapter's belief.
func TestOKXUserData_AReplayedPushDoesNotRunTheViewBackwards(t *testing.T) {
	cap := &okxCapture{}
	view := newOKXOrders("o1")
	first := `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312","clOrdId":"o1","state":"partially_filled","fillSz":"0.4","fillPx":"50000","accFillSz":"0.4","tradeId":"21","fillFee":"-0.02","fillFeeCcy":"USDT","uTime":"1700000001000"}]}`
	second := `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312","clOrdId":"o1","state":"partially_filled","fillSz":"0.5","fillPx":"50100","accFillSz":"0.9","tradeId":"22","fillFee":"-0.02","fillFeeCcy":"USDT","uTime":"1700000002000"}]}`
	_ = okxIngesterOverView([][]byte{[]byte(first), []byte(second), []byte(first)}, cap, view).Run(context.Background())

	held := view.get(t, "o1")
	got := dec.FromProto(held.GetFilledQuantity())
	if got.Cmp(dec.Rat("0.9")) != 0 {
		t.Fatalf("after a replay of the first push the adapter believes filled_quantity is %s, want "+
			"0.9. The view is the expected side of reconcileOrders, so a regressed value makes the "+
			"healing watchdog find drift against an order that is fine and emit a StateHealed FACT "+
			"for it", dec.Str(got))
	}
	if n := len(view.errs); n != 1 {
		t.Errorf("the replayed push produced %d refusals on the view's error seam, want 1 — a "+
			"refusal nobody reported is one nobody can count or alert on", n)
	}
	if len(cap.events) != 3 {
		t.Errorf("published %d fill FACTs, want 3 — the replayed push is still published and "+
			"deduped downstream by fill_id", len(cap.events))
	}
}

// TestOKXUserData_AsOfCarriesUTime — uTime is OKX's ordering token, and the view
// must hold it rather than this process's clock. A replayed push carries its
// original uTime, which is the only thing that distinguishes it from a fresh one.
func TestOKXUserData_AsOfCarriesUTime(t *testing.T) {
	cap := &okxCapture{}
	view := newOKXOrders("o1")
	frame := `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312","clOrdId":"o1","state":"filled","fillSz":"1","fillPx":"50000","accFillSz":"1","tradeId":"31","fillFee":"-0.05","fillFeeCcy":"USDT","uTime":"1700000009000"}]}`
	_ = okxIngesterOverView([][]byte{[]byte(frame)}, cap, view).Run(context.Background())

	if got := view.get(t, "o1").GetAsOf().AsTime().UnixMilli(); got != 1700000009000 {
		t.Errorf("the view's as_of is %d, want 1700000009000 (uTime). as_of is what Progress orders "+
			"reports by; a local clock there makes every replay look newer than what it replaces", got)
	}
}
