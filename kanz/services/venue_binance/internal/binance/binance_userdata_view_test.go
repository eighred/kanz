package binance

import (
	"context"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// THE FILL PATH WRITES BACK TO THE ADAPTER'S OWN ORDER VIEW (#904).
//
// The ingester always computed the healed OrderState and published it as a FACT.
// What it had nowhere to put was the state itself: OrderLookup was read-only, so
// the only writer that ever moved an order to a terminal status was the
// venue-confirmed-cancel branch of the gRPC CancelOrder handler. An order that
// FILLED stayed at whatever the OMS handed Execute for the life of the process,
// and the reconciler re-queried it at the exchange and re-emitted StateHealed
// about it on every pass, forever.
//
// These tests hold both halves of the repair, and the second is the one that
// matters more: a FILLED order must leave the view's open set, and a
// PARTIALLY_FILLED one must NOT, because it is still live at the exchange.

const filledReport = `{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"FILLED",` +
	`"l":"1","L":"50000","z":"1","q":"1","t":7,"T":1700000000000}`

const partialReport = `{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"PARTIALLY_FILLED",` +
	`"l":"0.4","L":"50000","z":"0.4","q":"1","t":7,"T":1700000000000}`

func TestUserData_AFilledReportMarksTheOrderTerminalInTheView(t *testing.T) {
	view := newFakeOrders("o1")
	cap := &reconCapture{}
	_ = ingesterOverView([][]byte{[]byte(filledReport)}, cap, view).Run(context.Background())

	if len(view.errs) != 0 {
		t.Fatalf("the view reported %v while recording the fill", view.errs)
	}
	if got := view.get(t, "o1").GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("the adapter's view has order o1 at %v after a FILLED report, want FILLED — it "+
			"disagrees with the order.order.filled FACT it just published (#904)", got)
	}
	if open := view.openIDs(t); len(open) != 0 {
		t.Fatalf("the view still believes %v are open at the venue — the reconciler will re-query "+
			"them and re-emit StateHealed on every pass, forever (#904)", open)
	}
}

// THE ONE THAT MUST NOT REGRESS. A partial fill is a LIVE order; calling it
// terminal would take it out of ExpectedOrders and blind the healing watchdog to
// an order still working at the exchange — worse than the leak being closed.
func TestUserData_APartialFillLeavesTheOrderOpenInTheView(t *testing.T) {
	view := newFakeOrders("o1")
	cap := &reconCapture{}
	_ = ingesterOverView([][]byte{[]byte(partialReport)}, cap, view).Run(context.Background())

	if len(view.errs) != 0 {
		t.Fatalf("the view reported %v while recording the partial fill", view.errs)
	}
	st := view.get(t, "o1")
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		t.Fatalf("order o1 is at %v after a PARTIALLY_FILLED report, want PARTIALLY_FILLED — "+
			"Binance's own X field is the verdict here, not an inference from quantities",
			st.GetStatus())
	}
	open := view.openIDs(t)
	if len(open) != 1 || open[0] != "o1" {
		t.Fatalf("the view believes %v are open after a PARTIAL fill, want [o1] — a live order "+
			"has disappeared from the healing watchdog, which is a worse defect than #904", open)
	}
}

// THE CONSEQUENCE, MEASURED AT THE EXCHANGE. A second reconciliation pass over a
// filled order must issue NO query-order and emit NO further StateHealed. Before
// this fix the same pass spent REST weight and published a duplicate FACT about
// the order every minute for as long as the process lived.
func TestRecon_AFilledOrderIsNeitherRequeriedNorRehealed(t *testing.T) {
	view := newFakeOrders("o1")
	cap := &reconCapture{}
	_ = ingesterOverView([][]byte{[]byte(filledReport)}, cap, view).Run(context.Background())

	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","clientOrderId":"o1","status":"FILLED","executedQty":"1"}`
	rcap := &reconCapture{}
	bucket := newWeightBucket(1200, time.Minute, nil)
	rest := newBinanceREST(restConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Bucket: bucket})
	r := newReconciler(ReconcilerConfig{
		REST: rest, Symbols: StaticSymbolMap{"BTC-USD": "BTCUSDT"},
		Expected: view, Pub: rcap, Venue: "BINANCE", Tenant: "fund-alpha",
	})

	for pass := range 2 {
		if err := r.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile pass %d: %v", pass, err)
		}
	}

	if f.gets != 0 {
		t.Fatalf("the reconciler issued %d query-order calls for an order that already filled — "+
			"that is REST weight, on a rate-limited budget, spent on an order that is finished "+
			"(#904)", f.gets)
	}
	for _, e := range rcap.events {
		if _, ok := e.Payload.(*orderpb.StateHealed); ok {
			t.Fatal("a StateHealed FACT was emitted for an order that already filled — the event " +
				"spine repeats itself about an order that finished (#904)")
		}
	}
}
