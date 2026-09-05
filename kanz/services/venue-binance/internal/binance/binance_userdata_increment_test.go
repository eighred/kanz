package binance

// THE FILL QUANTITY IS AN INCREMENT, AND NOTHING PROVED IT (#1046).
//
// The OMS order aggregate ADDS fill.quantity to filled_quantity
// (services/oms/internal/order/aggregate.go), so what this ingester publishes
// must be the LAST executed quantity (Binance's l) and never the cumulative
// (z). Both are on every executionReport, one identifier apart.
//
// Every websocket fixture in this package had l == z, and no fixture anywhere
// fed two sequential partials on one order. So the property held by
// construction and was pinned by nothing: swapping parseDec(rep.LastQty) for
// parseDec(rep.CumQty) — a systematic double-count of every partially filled
// order, straight into the position book and the accounting ledger — left
// ./services/venue-binance/... FULLY GREEN. Measured on main before this file
// existed, not inferred.
//
// The fixture below is the smallest thing that cannot pass under that edit: two
// partials on ONE order where l differs from z.

import (
	"context"
	"testing"

	"github.com/eighred/kanz/internal/dec"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// TestUserData_TwoSequentialPartialsPublishIncrementsNotCumulatives.
//
// An order for 1 filling in two legs: 0.4 then 0.6, cumulative 0.4 then 1.0.
// The two published Fill.quantity values must be 0.4 and 0.6 — the increments —
// and they must sum to the final cumulative. Under the CumQty mutation they are
// 0.4 and 1.0, summing to 1.4: forty percent of a position the fund never
// bought, booked and journalled with a straight face.
//
// The quantities are small and exact on purpose. bdec is dec.ToProto, which
// WRAPS on large values while production parses through parseDec (#1058), so a
// fixture built on big numbers would be measuring the helper.
func TestUserData_TwoSequentialPartialsPublishIncrementsNotCumulatives(t *testing.T) {
	cap := &reconCapture{}
	frames := [][]byte{
		[]byte(`{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"PARTIALLY_FILLED","l":"0.4","L":"50000","z":"0.4","q":"1","t":11,"T":1700000001000,"E":1700000001000}`),
		[]byte(`{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"FILLED","l":"0.6","L":"50100","z":"1","q":"1","t":12,"T":1700000002000,"E":1700000002000}`),
	}
	_ = ingesterOver(frames, cap).Run(context.Background()) // io.EOF at the end of the frames

	var quantities []*orderpb.Fill
	var finalCumulative *orderpb.OrderState
	for _, e := range cap.events {
		switch p := e.Payload.(type) {
		case *orderpb.OrderPartiallyFilled:
			quantities = append(quantities, p.GetFill())
		case *orderpb.OrderFilled:
			quantities = append(quantities, p.GetFill())
			finalCumulative = p.GetState()
		}
	}
	if len(quantities) != 2 {
		t.Fatalf("two TRADE reports on one order published %d fill FACTs, want 2", len(quantities))
	}

	// The increments, each one exactly what THAT report executed.
	for i, want := range []string{"0.4", "0.6"} {
		got := dec.FromProto(quantities[i].GetQuantity())
		if got.Cmp(dec.Rat(want)) != 0 {
			t.Errorf("fill %d published quantity %s, want %s. The OMS aggregate ADDS this to "+
				"filled_quantity, so publishing the report's CUMULATIVE (z) instead of its last "+
				"executed quantity (l) double-counts every partially filled order into the "+
				"position book and the ledger", i+1, dec.Str(got), want)
		}
	}

	// And the sum is the cumulative the venue itself reported — the property that
	// makes the increment the right thing to publish rather than merely a
	// different number.
	sum := dec.FromProto(quantities[0].GetQuantity())
	sum.Add(sum, dec.FromProto(quantities[1].GetQuantity()))
	cum := dec.FromProto(finalCumulative.GetFilledQuantity())
	if sum.Cmp(cum) != 0 {
		t.Errorf("the published increments sum to %s and Binance's final cumulative is %s. Those "+
			"must agree, or the position the OMS folds up from the fill stream is not the position "+
			"the exchange says it holds", dec.Str(sum), dec.Str(cum))
	}
	if cum.Cmp(dec.Rat("1")) != 0 {
		t.Errorf("the healed state carries cumulative %s, want 1 (z from the last report)", dec.Str(cum))
	}
}

// TestUserData_AsOfCarriesTheEventTimeNotTheTransactTime.
//
// as_of on this adapter's view is the ORDERING TOKEN orderview.Progress refuses
// stale reports by, and order.v1 defines it as "the event_time of the change
// that produced it" — Binance's E. It used to be stamped from T, the transact
// time, which is the trade's own instant and belongs on Fill.executed_at.
//
// The two differ here so a stamp taken from the wrong field cannot pass.
func TestUserData_AsOfCarriesTheEventTimeNotTheTransactTime(t *testing.T) {
	cap := &reconCapture{}
	view := newFakeOrders("o1")
	frame := `{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"FILLED","l":"1","L":"50000","z":"1","q":"1","t":21,"T":1700000001000,"E":1700000009000}`
	_ = ingesterOverView([][]byte{[]byte(frame)}, cap, view).Run(context.Background())

	held := view.get(t, "o1")
	if got := held.GetAsOf().AsTime().UnixMilli(); got != 1700000009000 {
		t.Errorf("the view's as_of is %d, want 1700000009000 (E, the event time). as_of is what "+
			"Progress orders reports by; stamping it from T conflates the trade's instant with the "+
			"stream's own clock and leaves the ordering gate comparing the wrong thing", got)
	}

	var executedAt int64
	for _, e := range cap.events {
		if f, ok := e.Payload.(*orderpb.OrderFilled); ok {
			executedAt = f.GetFill().GetExecutedAt().AsTime().UnixMilli()
		}
	}
	if executedAt != 1700000001000 {
		t.Errorf("Fill.executed_at is %d, want 1700000001000 (T, the transact time). The ledger and "+
			"TCA date the execution by when it TRADED, not by when the stream mentioned it", executedAt)
	}
}

// TestUserData_AsOfFallsBackToTheTransactTimeWhenNoEventTime.
//
// A frame with no E must not stamp the view with the Unix epoch: every later
// report looks newer than 1970, so the ordering gate would be permanently
// disabled for that order while looking exactly like a working one.
func TestUserData_AsOfFallsBackToTheTransactTimeWhenNoEventTime(t *testing.T) {
	cap := &reconCapture{}
	view := newFakeOrders("o1")
	frame := `{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"FILLED","l":"1","L":"50000","z":"1","q":"1","t":31,"T":1700000001000}`
	_ = ingesterOverView([][]byte{[]byte(frame)}, cap, view).Run(context.Background())

	if got := view.get(t, "o1").GetAsOf().AsTime().UnixMilli(); got != 1700000001000 {
		t.Errorf("with no E the view's as_of is %d, want the transact time 1700000001000. Stamping "+
			"the epoch here silently disables the ordering gate for this order", got)
	}
}

// TestUserData_AReplayedReportDoesNotRunTheViewBackwards is the end-to-end
// shape of #1046 through the real ingester and the real order view.
//
// The third frame is the FIRST report delivered again — the reconnect replay
// this whole issue is about. It must not move the adapter's belief.
func TestUserData_AReplayedReportDoesNotRunTheViewBackwards(t *testing.T) {
	cap := &reconCapture{}
	view := newFakeOrders("o1")
	first := `{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"PARTIALLY_FILLED","l":"0.4","L":"50000","z":"0.4","q":"1","t":41,"T":1700000001000,"E":1700000001000}`
	second := `{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"PARTIALLY_FILLED","l":"0.5","L":"50100","z":"0.9","q":"1","t":42,"T":1700000002000,"E":1700000002000}`
	_ = ingesterOverView([][]byte{[]byte(first), []byte(second), []byte(first)}, cap, view).Run(context.Background())

	held := view.get(t, "o1")
	got := dec.FromProto(held.GetFilledQuantity())
	if got.Cmp(dec.Rat("0.9")) != 0 {
		t.Fatalf("after a replay of the first report the adapter believes filled_quantity is %s, "+
			"want 0.9. The view is the expected side of reconcileOrders, so a regressed value makes "+
			"the healing watchdog find drift against an order that is fine and emit a StateHealed "+
			"FACT for it", dec.Str(got))
	}
	if n := len(view.errs); n != 1 {
		t.Errorf("the replayed report produced %d refusals on the view's error seam, want 1 — a "+
			"refusal nobody reported is one nobody can count or alert on", n)
	}
	// The FACT stream is deliberately NOT asserted to have dropped it: the fill is
	// an increment with a deterministic fill_id, and all four consumers dedup on
	// that id. Only the adapter's own belief is at stake here.
	if len(cap.events) != 3 {
		t.Errorf("published %d fill FACTs, want 3 — the replayed report is still published and "+
			"deduped downstream by fill_id", len(cap.events))
	}
}
