package okx

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// THE FILL PATH WRITES BACK TO THE ADAPTER'S OWN ORDER VIEW (#904).
//
// Identical shape to venue-binance's, because the defect was identical in both
// adapters: the ingester computed the healed OrderState, published it as a FACT,
// and had nowhere to put it. See binance_userdata_view_test.go for the full
// argument; these hold the same two properties for OKX's private orders channel.

const okxFilledFrame = `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312",` +
	`"clOrdId":"o1","state":"filled","fillSz":"1","fillPx":"50000","accFillSz":"1","tradeId":"7",` +
	`"fillFee":"-0.05","fillFeeCcy":"USDT","uTime":"1700000000000"}]}`

const okxPartialFrame = `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312",` +
	`"clOrdId":"o1","state":"partially_filled","fillSz":"0.4","fillPx":"50000","accFillSz":"0.4",` +
	`"tradeId":"7","fillFee":"-0.02","fillFeeCcy":"USDT","uTime":"1700000000000"}]}`

func okxIngesterOverView(frames [][]byte, cap *okxCapture, view *okxOrders) *OKXUserDataIngester {
	return newOKXUserDataIngester(&okxStream{frames: frames}, view, cap, "OKX", "fund-alpha")
}

func TestOKXUserData_AFilledPushMarksTheOrderTerminalInTheView(t *testing.T) {
	view := newOKXOrders("o1")
	cap := &okxCapture{}
	_ = okxIngesterOverView([][]byte{[]byte(okxFilledFrame)}, cap, view).Run(context.Background())

	if len(view.errs) != 0 {
		t.Fatalf("the view reported %v while recording the fill", view.errs)
	}
	if got := view.get(t, "o1").GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("the adapter's view has order o1 at %v after a filled push, want FILLED — it "+
			"disagrees with the order.order.filled FACT it just published (#904)", got)
	}
	if open := view.openIDs(t); len(open) != 0 {
		t.Fatalf("the view still believes %v are open at the venue — the reconciler will re-query "+
			"them and re-emit StateHealed on every pass, forever (#904)", open)
	}
}

// THE ONE THAT MUST NOT REGRESS. A partially filled order is still live at OKX.
func TestOKXUserData_APartialFillLeavesTheOrderOpenInTheView(t *testing.T) {
	view := newOKXOrders("o1")
	cap := &okxCapture{}
	_ = okxIngesterOverView([][]byte{[]byte(okxPartialFrame)}, cap, view).Run(context.Background())

	if len(view.errs) != 0 {
		t.Fatalf("the view reported %v while recording the partial fill", view.errs)
	}
	if got := view.get(t, "o1").GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		t.Fatalf("order o1 is at %v after a partially_filled push, want PARTIALLY_FILLED — OKX's "+
			"own state field is the verdict here, not an inference from accFillSz", got)
	}
	open := view.openIDs(t)
	if len(open) != 1 || open[0] != "o1" {
		t.Fatalf("the view believes %v are open after a PARTIAL fill, want [o1] — a live order "+
			"has disappeared from the healing watchdog, which is a worse defect than #904", open)
	}
}

// THE CONSEQUENCE, MEASURED AT THE EXCHANGE.
func TestOKXRecon_AFilledOrderIsNeitherRequeriedNorRehealed(t *testing.T) {
	view := newOKXOrders("o1")
	cap := &okxCapture{}
	_ = okxIngesterOverView([][]byte{[]byte(okxFilledFrame)}, cap, view).Run(context.Background())

	f := newFakeOKX(t)
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled","accFillSz":"1","avgPx":"50000"}]}`
	rcap := &okxCapture{}
	bucket := NewWeightBucket(60, time.Minute, nil)
	rest := newOKXREST(okxRestConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Passphrase: "p", Buckets: newOKXBuckets(bucket, nil), Mode: exchangeauth.OKXDemo})
	r := newOKXReconciler(OKXReconcilerConfig{
		REST: rest, Symbols: StaticSymbolMap{"BTC-USD": "BTC-USDT"},
		Expected: view, Pub: rcap, Venue: "OKX", Tenant: "fund-alpha",
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
