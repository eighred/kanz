package okx

import (
	"context"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

// A VENUE THAT REPORTS MORE FILLED THAN WAS SENT IS REFUSED, NOT BOOKED (#1045).
//
// The identical defect, in the identical shape, one connector over — which is
// why the bound lives in execution.LeavesRemaining and not in either ingester.
// Both built leaves as ordered minus the venue's cumulative through SubDec, and
// SubDec represents a negative result and reports ok, so an accFillSz of 14
// against an order of 10 published an OrderFilled FACT with LeavesQuantity -4
// into the position book and the accounting ledger.
func TestOKXUserData_OverfillReportIsRefusedAndQuarantined(t *testing.T) {
	cap := &okxCapture{}
	view := newOKXOrders()
	ordered := okxKanzOrder("o1")
	ordered.OrderedQuantity = odec("10")
	if err := view.store.Record(context.Background(), ordered); err != nil {
		t.Fatalf("seed view: %v", err)
	}

	var refusals []string
	ing := newOKXUserDataIngester(OKXUserDataConfig{
		Stream: &okxStream{frames: [][]byte{[]byte(
			`{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312","clOrdId":"o1",` +
				`"state":"filled","fillSz":"4","fillPx":"50000","accFillSz":"14","tradeId":"7",` +
				`"fillFee":"-0.05","fillFeeCcy":"USDT","uTime":"1700000000000"}]}`)}},
		Orders: view, Pub: cap, Venue: "OKX", Tenant: "fund-alpha",
		OnRefused: func(mic, orderID, reason string) {
			refusals = append(refusals, mic+"/"+orderID+"/"+reason)
		},
	})
	_ = ing.Run(context.Background())

	for _, e := range cap.events {
		switch e.Payload.(type) {
		case *orderpb.OrderFilled, *orderpb.OrderPartiallyFilled:
			t.Fatalf("the over-fill report was PUBLISHED as %s — both books of record fold this "+
				"subject, and the state it carries claims a cumulative 14 against an order of 10",
				e.Subject)
		}
	}
	if len(refusals) != 1 {
		t.Fatalf("OnRefused fired %d times (%v), want exactly one", len(refusals), refusals)
	}
	st := view.get(t, "o1")
	if st.GetQuarantine() == nil {
		t.Fatal("the order is not quarantined in the adapter's own view — a refused report that " +
			"leaves the order workable is a silent drop")
	}
	if st.GetQuarantine().GetReason() == "" {
		t.Error("the quarantine carries no reason")
	}
	if dec.FromProto(st.GetFilledQuantity()).Sign() != 0 {
		t.Errorf("filled_quantity = %v, want the untouched zero — the report was refused",
			st.GetFilledQuantity())
	}
	if dec.FromProto(st.GetLeavesQuantity()).Sign() < 0 {
		t.Errorf("leaves_quantity = %v is NEGATIVE", st.GetLeavesQuantity())
	}
}

// THE BOUND IS NOT A CEILING ON ORDINARY FILLS.
func TestOKXUserData_AnExactFillIsNotMistakenForAnOverfill(t *testing.T) {
	cap := &okxCapture{}
	view := newOKXOrders()
	ordered := okxKanzOrder("o1")
	ordered.OrderedQuantity = odec("10")
	if err := view.store.Record(context.Background(), ordered); err != nil {
		t.Fatalf("seed view: %v", err)
	}
	refused := 0
	ing := newOKXUserDataIngester(OKXUserDataConfig{
		Stream: &okxStream{frames: [][]byte{[]byte(
			`{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312","clOrdId":"o1",` +
				`"state":"filled","fillSz":"10","fillPx":"50000","accFillSz":"10","tradeId":"9",` +
				`"fillFee":"-0.05","fillFeeCcy":"USDT","uTime":"1700000000000"}]}`)}},
		Orders: view, Pub: cap, Venue: "OKX", Tenant: "fund-alpha",
		OnRefused: func(string, string, string) { refused++ },
	})
	_ = ing.Run(context.Background())

	if refused != 0 {
		t.Fatalf("an exact fill was refused %d time(s) — the bound is cumulative > ordered, not >=", refused)
	}
	var filled *orderpb.OrderFilled
	for _, e := range cap.events {
		if f, ok := e.Payload.(*orderpb.OrderFilled); ok {
			filled = f
		}
	}
	if filled == nil {
		t.Fatal("an order that filled exactly published no OrderFilled")
	}
	if view.get(t, "o1").GetQuarantine() != nil {
		t.Error("an ordinary complete fill quarantined the order")
	}
}
