package binance

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
// not. It is the exact fault #1047 is about: the row is almost certainly there,
// and this adapter cannot see it.
//
// It wraps a real orderview.Memory rather than stubbing every method, so the
// order genuinely IS in the view — the test is about an adapter that owns the
// order and cannot read it, not about one that never held it.
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

// unreadableOrders is the adapter's real Seam over that store.
type unreadableOrders struct {
	*orderview.Seam
	store *unreadableStore
	errs  []error
}

func newUnreadableOrders(ids ...string) *unreadableOrders {
	f := &unreadableOrders{store: &unreadableStore{Memory: orderview.NewMemory()}}
	f.Seam = orderview.NewSeam(f.store, func(err error) { f.errs = append(f.errs, err) })
	for _, id := range ids {
		if err := f.store.Memory.Record(context.Background(), kanzOrder(id)); err != nil {
			panic(err)
		}
	}
	f.store.fail = true
	return f
}

// AN UNREADABLE ORDER VIEW MUST NOT READ AS "NOT OUR ORDER" (#1047).
//
// # What this pins
//
// Seam.Lookup collapsed a store failure into the same (nil, false) it returns
// for an order this adapter does not hold, and the ingester answered that
// collapsed value with `return nil` — the report consumed and forgotten. No
// order.order.filled FACT reaches the bus, so the position book never books the
// position and the accounting ledger never journals the cash. A Postgres blip
// in one venue adapter silently deletes real executions from the fill stream.
//
// The two answers are not the same answer. "The view says I do not hold this
// order" is a legitimate skip — the exchange account may be shared, or the
// report may belong to another replica — and it stays a skip. "I could not read
// the view" is the opposite: this adapter almost certainly DOES own the order
// and cannot tell, so treating it as somebody else's is applying the safe
// reading of the first case to the second.
//
// # What the ingester must do instead
//
// Refuse the report, count it under store_error, and return the error rather
// than swallowing it. The error is what makes the fault reach the connector's
// run loop instead of being absorbed frame by frame; the counter is what
// answers "how many fills did we lose during that outage?", which a log line
// cannot.
func TestUserData_StoreErrorIsNotASkip(t *testing.T) {
	cap := &reconCapture{}
	view := newUnreadableOrders("o1")

	var dropped []string
	ing := newUserDataIngester(UserDataConfig{
		Stream: &fakeStream{frames: [][]byte{[]byte(
			`{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"FILLED",` +
				`"l":"1","L":"50000","z":"1","q":"1","t":7,"T":1700000000000}`)}},
		Orders: view, Pub: cap, Venue: "BINANCE", Tenant: "fund-alpha",
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
	if got, want := dropped[0], "BINANCE/o1/"+DropStoreError; got != want {
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

// THE BENIGN SKIP IS STILL A SKIP, and it is counted separately.
//
// A report for an order this adapter never dispatched is not a fault: shared
// exchange accounts and multi-replica deployments both produce them routinely.
// It must not return an error — doing so would tear the websocket down on every
// frame belonging to somebody else — but it is still an execution this adapter
// did not turn into a FACT, so it is counted under its own reason. The two
// reasons together are what make the store_error series readable: an operator
// comparing them can tell "we are seeing other people's orders" from "we are
// blind".
func TestUserData_StoreErrorReasonIsNotUsedForAnUnknownOrder(t *testing.T) {
	cap := &reconCapture{}
	view := newFakeOrders() // empty view, reads fine

	var dropped []string
	ing := newUserDataIngester(UserDataConfig{
		Stream: &fakeStream{frames: [][]byte{[]byte(
			`{"e":"executionReport","s":"BTCUSDT","c":"somebody-elses","S":"BUY","x":"TRADE","X":"FILLED",` +
				`"l":"1","L":"50000","z":"1","q":"1","t":7,"T":1700000000000}`)}},
		Orders: view, Pub: cap, Venue: "BINANCE", Tenant: "fund-alpha",
		OnDropped: func(mic, orderID, reason string) {
			dropped = append(dropped, mic+"/"+orderID+"/"+reason)
		},
	})
	err := ing.Run(context.Background())

	if errors.Is(err, errUnreadableView) {
		t.Fatalf("an order this adapter does not hold returned a store fault (%v) — a shared "+
			"exchange account would tear the websocket down on every frame that is not ours", err)
	}
	if len(dropped) != 1 {
		t.Fatalf("OnDropped fired %d times (%v), want exactly one", len(dropped), dropped)
	}
	if got, want := dropped[0], "BINANCE/somebody-elses/"+DropUnknownOrder; got != want {
		t.Errorf("OnDropped recorded %q, want %q", got, want)
	}
}
