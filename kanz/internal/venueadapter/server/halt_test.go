package server

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
)

// THE LAST LINE BEFORE THE EXCHANGE (#635).
//
// Both venue adapters serve this shell, and until this neither of them heard the
// platform kill-switch: an operator's kanz-halt stopped TradingView signals
// while whatever the OMS routed was still placed at Binance and OKX.
//
// It is not redundant with the OMS's own refusal. The OMS refuses NEW orders; it
// does not stop what it does to orders it ALREADY admitted — the sweep re-drives
// an interrupted order, resume() re-works one after a crash, and a scheduled
// parent slices children on its own timer. Each of those reaches Execute without
// passing admission again, which is why the check lives here too.

// haltedVenueServer wires the shell with a gate closed by a real
// lifecycle.v1.ModeChanged FACT, decoded through the same Gate.Handle the bus
// subscription calls.
func haltedVenueServer(t *testing.T, v *fakeVenue) (*Server, orderview.Store) {
	t.Helper()
	g := halt.OpenGate(nil)
	payload, err := proto.Marshal(&lifecyclepb.ModeChanged{
		Component: halt.ComponentSystem,
		NewMode:   lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
		ChangedBy: "operator:akif",
		Reason:    "risk breach on fund-alpha",
	})
	if err != nil {
		t.Fatalf("marshal ModeChanged: %v", err)
	}
	if err := g.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Gate.Handle: %v", err)
	}
	closes := execution.NewCloseRegistry()
	v.closes = closes
	view := orderview.NewMemory()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(v, view, closes, execution.AccountProof{Verified: true, ExchangeAccountID: "12345678"}, g, logger), view
}

func TestExecuteRefusesToPlaceAnOrderWhileThePlatformIsHalted(t *testing.T) {
	v := &fakeVenue{fills: []*orderpb.Fill{{FillId: "f1"}}}
	s, view := haltedVenueServer(t, v)

	resp, err := s.Execute(context.Background(), &venuepb.ExecuteRequest{State: order()})
	if err == nil {
		t.Fatalf("Execute succeeded during a declared halt (resp: %v)", resp)
	}
	// FailedPrecondition, not Unavailable: the OMS must not read a halt as a venue
	// outage to heal around.
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", got)
	}
	if !strings.Contains(err.Error(), "risk breach on fund-alpha") {
		t.Fatalf("error = %v, want it to carry the operator's reason", err)
	}
	// THE ORDER WAS NOT RECORDED. The check sits BEFORE view.Record on purpose:
	// an order this adapter will not place must not enter the view as one it has,
	// or the reconciler and the fill-enrichment path both start believing in an
	// order the exchange never saw.
	if _, ok, gerr := view.Get(context.Background(), "ORD-1"); gerr != nil || ok {
		t.Fatal("a refused order was recorded in the adapter's order view — the reconciler will " +
			"now look for an order the exchange has never heard of")
	}
}

// THE EXITS STAY OPEN. A halt is not a cancel, and an operator halting on a risk
// breach must still be able to withdraw what is resting at the exchange.
func TestCancelOrderStillWorksWhileThePlatformIsHalted(t *testing.T) {
	v := &fakeVenue{}
	s, _ := haltedVenueServer(t, v)

	if _, err := s.CancelOrder(context.Background(), &venuepb.CancelOrderRequest{State: order()}); err != nil {
		t.Fatalf("cancel during a halt: %v — a brake that jams the exits is worse than no brake", err)
	}
	if v.trackedWhenCancelled != 1 {
		t.Fatalf("the cancel was not dispatched to the venue during the halt (tracked = %d)", v.trackedWhenCancelled)
	}
}

// DENY-BY-DEFAULT. An adapter whose gate was never wired, or that has not yet
// heard a ModeChanged, places nothing — the state every pod boots in.
func TestExecuteRefusesWhenTheAdapterHasNoGate(t *testing.T) {
	v := &fakeVenue{}
	closes := execution.NewCloseRegistry()
	v.closes = closes
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(v, orderview.NewMemory(), closes, execution.AccountProof{Verified: true}, nil, logger)

	if _, err := s.Execute(context.Background(), &venuepb.ExecuteRequest{State: order()}); err == nil {
		t.Fatal("an adapter with NO halt gate placed an order — a caller that forgot to wire the " +
			"brake does not get to trade")
	} else if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", got)
	}
}
