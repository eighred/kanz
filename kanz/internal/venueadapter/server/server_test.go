package server

// Tests for the gRPC shell's two dangerous contracts — the ones a naive RPC
// wrapper destroys silently:
//
//  1. A close must be TRACKED BEFORE it is dispatched. The healing watchdog exists
//     for the ambiguous cases (timeout, process death mid-call); tracking only on
//     success covers none of them.
//  2. A failed Execute must NEVER return fills. A fabricated fill books a trade
//     that never happened.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	venuepb "github.com/kanz-eng/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
)

// fakeVenue records the order in which the shell called it.
type fakeVenue struct {
	fills     []*orderpb.Fill
	execErr   error
	cancelErr error
	// trackedWhenCancelled is what the registry held AT THE MOMENT the venue
	// cancel was dispatched — the whole point of the In-Flight Certainty seam.
	trackedWhenCancelled int
	closes               *execution.CloseRegistry
}

func (f *fakeVenue) MIC() string     { return "XBIN" }
func (f *fakeVenue) Account() string { return "binance-main" }

func (f *fakeVenue) Execute(context.Context, *orderpb.OrderState) ([]*orderpb.Fill, error) {
	if f.execErr != nil {
		return nil, f.execErr
	}
	return f.fills, nil
}

func (f *fakeVenue) CancelOrder(context.Context, *orderpb.OrderState) error {
	f.trackedWhenCancelled = f.closes.Len()
	return f.cancelErr
}

func newServer(t *testing.T, v *fakeVenue) (*Server, *execution.CloseRegistry, orderview.Store) {
	t.Helper()
	closes := execution.NewCloseRegistry()
	v.closes = closes
	view := orderview.NewMemory()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(v, view, closes, execution.AccountProof{Verified: true, ExchangeAccountID: "12345678"}, logger), closes, view
}

func order() *orderpb.OrderState {
	return &orderpb.OrderState{OrderId: "ORD-1", InstrumentId: "BTC-USD"}
}

func TestCancelTracksTheCloseBEFOREDispatchingIt(t *testing.T) {
	v := &fakeVenue{}
	s, _, _ := newServer(t, v)

	if _, err := s.CancelOrder(context.Background(), &venuepb.CancelOrderRequest{State: order()}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// The venue saw the intent already registered when it was called. If this is
	// 0, a cancel that hangs or dies mid-flight leaves NOTHING for the healing
	// watchdog to find, and the order stays live at the exchange forever.
	if v.trackedWhenCancelled != 1 {
		t.Fatalf("registry held %d closes when the venue cancel was dispatched, want 1 (tracked BEFORE dispatch)", v.trackedWhenCancelled)
	}
}

func TestCancelLeavesTheCloseInFlightWhenTheVenueErrors(t *testing.T) {
	// An ambiguous cancel (timeout) must stay tracked — it is exactly the case the
	// watchdog exists to resolve. Resolving it here would tell the OMS an order was
	// withdrawn while it may still be live at the exchange.
	v := &fakeVenue{cancelErr: errors.New("timeout")}
	s, closes, _ := newServer(t, v)

	if _, err := s.CancelOrder(context.Background(), &venuepb.CancelOrderRequest{State: order()}); err == nil {
		t.Fatal("cancel: an ambiguous venue failure was reported as a confirmed withdrawal")
	}
	if closes.Len() != 1 {
		t.Fatalf("registry holds %d closes after a failed cancel, want 1 — the healing watchdog has nothing to heal", closes.Len())
	}
}

func TestCancelResolvesTheCloseOnlyWhenTheVenueConfirms(t *testing.T) {
	v := &fakeVenue{}
	s, closes, _ := newServer(t, v)

	if _, err := s.CancelOrder(context.Background(), &venuepb.CancelOrderRequest{State: order()}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if closes.Len() != 0 {
		t.Fatalf("registry still holds %d closes after a CONFIRMED cancel, want 0 (leak — the watchdog would re-heal it forever)", closes.Len())
	}
}

func TestExecuteRecordsTheOrderBeforeWorkingIt(t *testing.T) {
	// A marketable order can fill on the websocket before Execute even returns. The
	// user-data ingester enriches that fill by reading the order view — so if the
	// order is recorded AFTER the venue call, the first fill of a fast order finds
	// nothing to enrich.
	v := &fakeVenue{}
	s, _, view := newServer(t, v)

	if _, err := s.Execute(context.Background(), &venuepb.ExecuteRequest{State: order()}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, ok, _ := view.Get(context.Background(), "ORD-1"); !ok {
		t.Fatal("order was not recorded in the adapter's own view — its fills would be unenrichable")
	}
}

func TestExecuteNeverReturnsFillsWhenTheVenueFails(t *testing.T) {
	v := &fakeVenue{execErr: errors.New("exchange unreachable")}
	s, _, _ := newServer(t, v)

	resp, err := s.Execute(context.Background(), &venuepb.ExecuteRequest{State: order()})
	if err == nil {
		t.Fatal("execute: want an error when the exchange is unreachable")
	}
	if len(resp.GetFills()) != 0 {
		t.Fatalf("fabricated %d fills on a failed execute — that books a trade that never happened", len(resp.GetFills()))
	}
}

func TestExecuteRestingOrderIsNotAnError(t *testing.T) {
	// Zero fills = a limit order that rests unfilled. Valid. Turning it into an
	// error makes the OMS treat a working order as a failed one.
	v := &fakeVenue{fills: nil}
	s, _, _ := newServer(t, v)

	resp, err := s.Execute(context.Background(), &venuepb.ExecuteRequest{State: order()})
	if err != nil {
		t.Fatalf("a resting order was reported as a failure: %v", err)
	}
	if len(resp.GetFills()) != 0 {
		t.Fatalf("got %d fills, want 0", len(resp.GetFills()))
	}
}
