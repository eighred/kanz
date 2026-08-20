package execution

import (
	"context"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// THE ZERO VALUE MUST BE THE SAFE ANSWER.
//
// A caller that constructs an OrderView and forgets to set State, or a venue
// that returns OrderView{} on a path nobody thought about, must produce the
// answer that FREEZES the order — never the one that says "the venue does not
// have it", which is the answer that authorizes re-driving it to the exchange.
// This mirrors AccountProof (venue.go): the unchecked adapter reports the
// safe answer, not the flattering one.
func TestZeroOrderViewIsIndeterminateNotUnknown(t *testing.T) {
	var v OrderView
	if v.State != OrderViewIndeterminate {
		t.Fatalf("zero OrderView.State is %v, want OrderViewIndeterminate", v.State)
	}
	if OrderViewIndeterminate == OrderViewUnknown {
		t.Fatal("OrderViewIndeterminate and OrderViewUnknown are the same value — " +
			"then a forgotten field means 'the venue never saw this order', which is " +
			"the one answer that authorizes sending it to an exchange again")
	}
}

// Every state must print as something an operator can read in a quarantine
// reason. A bare integer in an incident log costs the reader a trip to the
// source at the worst possible moment.
func TestOrderViewStateStrings(t *testing.T) {
	for _, tc := range []struct {
		state OrderViewState
		want  string
	}{
		{OrderViewIndeterminate, "INDETERMINATE"},
		{OrderViewUnknown, "UNKNOWN"},
		{OrderViewWorking, "WORKING"},
		{OrderViewPartiallyFilled, "PARTIALLY_FILLED"},
		{OrderViewFilled, "FILLED"},
		{OrderViewRejected, "REJECTED"},
		{OrderViewState(99), "OrderViewState(99)"},
	} {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("OrderViewState(%d).String() = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// queryingVenue proves the interface is satisfiable by an ordinary venue and
// that a Venue can be type-asserted to Querier — the assertion the OMS makes.
type queryingVenue struct {
	Venue
	view OrderView
}

func (q queryingVenue) QueryOrder(context.Context, *orderpb.OrderState) (OrderView, error) {
	return q.view, nil
}

func TestQuerierIsTypeAssertableFromVenue(t *testing.T) {
	var v Venue = queryingVenue{Venue: NewSimVenue("XSIM"), view: OrderView{State: OrderViewWorking}}
	q, ok := v.(Querier)
	if !ok {
		t.Fatal("a Venue implementing QueryOrder does not satisfy Querier")
	}
	got, err := q.QueryOrder(context.Background(), &orderpb.OrderState{OrderId: "o-1"})
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if got.State != OrderViewWorking {
		t.Fatalf("view state = %v, want OrderViewWorking", got.State)
	}
}
