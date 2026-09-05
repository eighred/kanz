package okx

import (
	"context"
	"errors"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/venueadapter/orderview"
)

// errUnreadableView is what a Postgres blip looks like to the order view.
var errUnreadableView = errors.New("orderview: connection refused")

// unreadableStore is an orderview.Store whose READ fails and whose writes do
// not — the exact fault #1047 is about. It wraps a real orderview.Memory rather
// than stubbing every method, so the order genuinely IS in the view: the test is
// about an adapter that owns the order and cannot see it, not one that never
// held it.
type unreadableStore struct {
	*orderview.Memory
	fail bool
}

func (s *unreadableStore) Get(ctx context.Context, orderID string) (*orderpb.OrderState, orderview.Revision, bool, error) {
	if s.fail {
		return nil, orderview.Revision{}, false, errUnreadableView
	}
	return s.Memory.Get(ctx, orderID)
}

type unreadableOrders struct {
	*orderview.Seam
	store *unreadableStore
	errs  []error
}

func newUnreadableOrders(ids ...string) *unreadableOrders {
	f := &unreadableOrders{store: &unreadableStore{Memory: orderview.NewMemory()}}
	f.Seam = orderview.NewSeam(f.store, func(err error) { f.errs = append(f.errs, err) })
	for _, id := range ids {
		if err := f.store.Memory.Record(context.Background(), okxKanzOrder(id)); err != nil {
			panic(err)
		}
	}
	f.store.fail = true
	return f
}

// AN UNREADABLE ORDER VIEW MUST NOT READ AS "NOT OUR ORDER" (#1047).
//
// The identical collapse, in the identical shape, one connector over — which is
// why the drop lives in execution.ReportRefusal.Dropped and the three-value
// answer lives in execution.OrderLookup, rather than either being written out
// twice. Both ingesters read Lookup's `ok` and answered a store failure with the
// skip they owe an order that belongs to somebody else, so a Postgres blip
// deleted real executions from the order.order.filled stream: no FACT, so no
// position booked and no cash journalled.
func TestOKXUserData_StoreErrorIsNotASkip(t *testing.T) {
	cap := &okxCapture{}
	view := newUnreadableOrders("o1")

	var dropped []string
	ing := newOKXUserDataIngester(OKXUserDataConfig{
		Stream: &okxStream{frames: [][]byte{[]byte(
			`{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","ordId":"312","clOrdId":"o1",` +
				`"state":"filled","fillSz":"1","fillPx":"50000","accFillSz":"1","tradeId":"7",` +
				`"fillFee":"-0.05","fillFeeCcy":"USDT","uTime":"1700000000000"}]}`)}},
		Orders: view, Pub: cap, Venue: "OKX", Tenant: "fund-alpha",
		OnDropped: func(mic, orderID, reason string) {
			dropped = append(dropped, mic+"/"+orderID+"/"+reason)
		},
	})

	err := ing.Run(context.Background())

	// 1. THE REPORT IS NOT SILENTLY CONSUMED.
	if err == nil {
		t.Fatal("Run returned nil for an execution report the adapter could not resolve — the " +
			"report was consumed and forgotten, so the fill it carried reaches no FACT, no " +
			"position projection and no ledger entry, and nothing downstream can notice")
	}
	if !errors.Is(err, errUnreadableView) {
		t.Errorf("Run returned %v, which does not wrap the store failure — an operator reading "+
			"this cannot tell an unreadable order view from any other stream fault", err)
	}

	// 2. IT IS COUNTED, AND UNDER store_error RATHER THAN THE BENIGN REASON.
	if len(dropped) != 1 {
		t.Fatalf("OnDropped fired %d times (%v), want exactly one — a lost execution nothing "+
			"counts cannot answer \"how many fills did we lose during that outage?\"",
			len(dropped), dropped)
	}
	if got, want := dropped[0], "OKX/o1/"+DropStoreError; got != want {
		t.Errorf("OnDropped recorded %q, want %q — an unreadable view counted as %q would be "+
			"indistinguishable from the routine skip of somebody else's order",
			got, want, DropUnknownOrder)
	}

	// 3. NOTHING WAS PUBLISHED ON A STATE NOBODY COULD READ.
	for _, e := range cap.events {
		switch e.Payload.(type) {
		case *orderpb.OrderFilled, *orderpb.OrderPartiallyFilled:
			t.Fatalf("a fill FACT (%s) was published from a report the adapter could not enrich — "+
				"its instrument and side would have come from a state that was never read", e.Subject)
		}
	}

	// 4. THE FAULT ALSO REACHED onErr, which is where the read-failure counter hangs.
	if len(view.errs) == 0 {
		t.Error("the seam reported no error to onErr — the orderview read-failure counter is " +
			"incremented there, so a silent seam makes the outage unalertable")
	}
}

// THE BENIGN SKIP IS STILL A SKIP, and it is counted under its own reason. An
// OKX push carries a BATCH of order updates, so the skip must also leave the
// rest of the frame processable — returning an error for somebody else's order
// would abandon the fills of ours that follow it in the same message.
func TestOKXUserData_StoreErrorReasonIsNotUsedForAnUnknownOrder(t *testing.T) {
	cap := &okxCapture{}
	view := newOKXOrders("o1") // holds o1, reads fine

	var dropped []string
	ing := newOKXUserDataIngester(OKXUserDataConfig{
		Stream: &okxStream{frames: [][]byte{[]byte(
			`{"arg":{"channel":"orders"},"data":[` +
				`{"instId":"BTC-USDT","ordId":"311","clOrdId":"somebody-elses","state":"filled",` +
				`"fillSz":"1","fillPx":"50000","accFillSz":"1","tradeId":"6",` +
				`"fillFee":"-0.05","fillFeeCcy":"USDT","uTime":"1700000000000"},` +
				`{"instId":"BTC-USDT","ordId":"312","clOrdId":"o1","state":"filled",` +
				`"fillSz":"1","fillPx":"50000","accFillSz":"1","tradeId":"7",` +
				`"fillFee":"-0.05","fillFeeCcy":"USDT","uTime":"1700000000000"}]}`)}},
		Orders: view, Pub: cap, Venue: "OKX", Tenant: "fund-alpha",
		OnDropped: func(mic, orderID, reason string) {
			dropped = append(dropped, mic+"/"+orderID+"/"+reason)
		},
	})
	err := ing.Run(context.Background())

	if errors.Is(err, errUnreadableView) {
		t.Fatalf("an order this adapter does not hold returned a store fault (%v)", err)
	}
	if len(dropped) != 1 {
		t.Fatalf("OnDropped fired %d times (%v), want exactly one", len(dropped), dropped)
	}
	if got, want := dropped[0], "OKX/somebody-elses/"+DropUnknownOrder; got != want {
		t.Errorf("OnDropped recorded %q, want %q", got, want)
	}
	var filled *orderpb.OrderFilled
	for _, e := range cap.events {
		if f, ok := e.Payload.(*orderpb.OrderFilled); ok {
			filled = f
		}
	}
	if filled == nil {
		t.Fatal("the fill for o1, which follows somebody else's order in the SAME push, was never " +
			"published — skipping an unknown order must not abandon the rest of the batch")
	}
}
