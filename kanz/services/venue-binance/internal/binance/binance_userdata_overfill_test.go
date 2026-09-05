package binance

import (
	"context"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

// A VENUE THAT REPORTS MORE FILLED THAN WAS SENT IS REFUSED, NOT BOOKED (#1045).
//
// # What this pins
//
// Three paths carry a fill into this platform. Two of them — the synchronous
// Execute return and the crash-recovery adopt — fold through the OMS aggregate's
// ApplyFill, whose OVERFILL refusal bounds the fill against the open quantity.
// The user-data websocket takes neither: the ingester builds the fill FACT here
// and publishes it, and the OMS order aggregate is not a consumer of that
// subject. The position projector and the accounting ledger are.
//
// So the arithmetic in applyFillToState was the only bound on this path and it
// was not a bound at all: leaves is ordered minus cumulative through SubDec,
// which represents a negative result perfectly well and reports ok. A venue
// reporting a cumulative 14 against an order of 10 published an OrderFilled FACT
// carrying FilledQuantity 14 and LeavesQuantity -4. Both books folded it. There
// was no refusal, no quarantine and no counter — the first thing that could
// notice was a balance reconciliation against the exchange, if it ran.
//
// # Why the refusal is not a drop
//
// Dropping the report would trade a wrong number for a missing execution, which
// is worse: the fund would hold a position with no FACT at all. So the refusal
// leaves three durable marks — the counter this asserts, an ERROR log, and a
// quarantine on the adapter's own view of the order, which is the same
// OrderQuarantine record the OMS writes and which orderview.Dispatch refuses to
// re-dispatch over.
func TestUserData_OverfillReportIsRefusedAndQuarantined(t *testing.T) {
	cap := &reconCapture{}
	view := newFakeOrders()
	// Ordered 10. The issue's own probe used these numbers.
	ordered := kanzOrder("o1")
	ordered.OrderedQuantity = bdec("10")
	if err := view.store.Record(context.Background(), ordered); err != nil {
		t.Fatalf("seed view: %v", err)
	}

	var refusals []string
	ing := newUserDataIngester(UserDataConfig{
		Stream: &fakeStream{frames: [][]byte{[]byte(
			`{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"FILLED",` +
				`"l":"4","L":"50000","z":"14","q":"10","t":7,"T":1700000000000}`)}},
		Orders: view, Pub: cap, Venue: "BINANCE", Tenant: "fund-alpha",
		OnRefused: func(mic, orderID, reason string) {
			refusals = append(refusals, mic+"/"+orderID+"/"+reason)
		},
	})
	_ = ing.Run(context.Background()) // io.EOF at the end of the frames

	for _, e := range cap.events {
		switch e.Payload.(type) {
		case *orderpb.OrderFilled, *orderpb.OrderPartiallyFilled:
			t.Fatalf("the over-fill report was PUBLISHED as %s — the position book and the "+
				"accounting ledger both fold this subject, and the state it carries claims a "+
				"cumulative 14 against an order of 10", e.Subject)
		}
	}

	if len(refusals) != 1 {
		t.Fatalf("OnRefused fired %d times (%v), want exactly one — a refusal nothing counts is "+
			"indistinguishable from a venue that never over-filled", len(refusals), refusals)
	}

	st := view.get(t, "o1")
	if st.GetQuarantine() == nil {
		t.Fatal("the order is not quarantined in the adapter's own view — a refused report that " +
			"leaves the order workable is a silent drop, and the platform would keep re-driving " +
			"an order the venue and the platform disagree about the size of")
	}
	if st.GetQuarantine().GetReason() == "" {
		t.Error("the quarantine carries no reason — an operator reading it has to reconstruct " +
			"the disagreement from the venue's own order history with no starting point")
	}
	// THE VIEW MUST NOT HAVE BEEN HEALED TO THE VENUE'S NUMBER. Recording filled
	// 14 against ordered 10 would make the adapter agree with a size the platform
	// never authorised, which is the same defect one layer in.
	if got := st.GetFilledQuantity(); dec.FromProto(got).Sign() != 0 {
		t.Errorf("filled_quantity = %v, want the untouched zero — the report was refused, so "+
			"nothing about it may reach the view", got)
	}
	if dec.FromProto(st.GetLeavesQuantity()).Sign() < 0 {
		t.Errorf("leaves_quantity = %v is NEGATIVE — this is the value the FACT would have "+
			"carried into the position book", st.GetLeavesQuantity())
	}
}

// A partial report whose cumulative exceeds the order is refused on the same
// terms: the subject differs, the two books that fold it do not.
func TestUserData_OverfillOnAPartialReportIsAlsoRefused(t *testing.T) {
	cap := &reconCapture{}
	view := newFakeOrders()
	ordered := kanzOrder("o1")
	ordered.OrderedQuantity = bdec("10")
	if err := view.store.Record(context.Background(), ordered); err != nil {
		t.Fatalf("seed view: %v", err)
	}
	refused := 0
	ing := newUserDataIngester(UserDataConfig{
		Stream: &fakeStream{frames: [][]byte{[]byte(
			`{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"PARTIALLY_FILLED",` +
				`"l":"1","L":"50000","z":"10.5","q":"10","t":8,"T":1700000000000}`)}},
		Orders: view, Pub: cap, Venue: "BINANCE", Tenant: "fund-alpha",
		OnRefused: func(string, string, string) { refused++ },
	})
	_ = ing.Run(context.Background())

	if len(cap.events) != 0 {
		t.Fatalf("a partially-filled report over the ordered quantity published %d event(s)", len(cap.events))
	}
	if refused != 1 {
		t.Fatalf("OnRefused fired %d times, want 1", refused)
	}
	if view.get(t, "o1").GetQuarantine() == nil {
		t.Fatal("the order is not quarantined")
	}
}

// THE BOUND IS NOT A CEILING ON ORDINARY FILLS. A report that fills the order
// exactly is the common case and must still publish — a guard that refused it
// would stop every completed order from being booked.
func TestUserData_AnExactFillIsNotMistakenForAnOverfill(t *testing.T) {
	cap := &reconCapture{}
	view := newFakeOrders()
	ordered := kanzOrder("o1")
	ordered.OrderedQuantity = bdec("10")
	if err := view.store.Record(context.Background(), ordered); err != nil {
		t.Fatalf("seed view: %v", err)
	}
	refused := 0
	ing := newUserDataIngester(UserDataConfig{
		Stream: &fakeStream{frames: [][]byte{[]byte(
			`{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"FILLED",` +
				`"l":"10","L":"50000","z":"10","q":"10","t":9,"T":1700000000000}`)}},
		Orders: view, Pub: cap, Venue: "BINANCE", Tenant: "fund-alpha",
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
	if dec.FromProto(filled.GetState().GetLeavesQuantity()).Sign() != 0 {
		t.Errorf("leaves = %v, want 0", filled.GetState().GetLeavesQuantity())
	}
	if view.get(t, "o1").GetQuarantine() != nil {
		t.Error("an ordinary complete fill quarantined the order")
	}
}
