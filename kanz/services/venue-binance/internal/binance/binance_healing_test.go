package binance

import (
	"context"
	"math/big"
	"testing"
	"time"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

// The Binance half of the In-Flight Certainty seam, mirroring the OKX healing
// suite: a close that stays unconfirmed past the 1500ms timeout is force-resolved
// against venue truth so the ledger never freezes.

func healReconOverBinance(f *fakeBinance, cap *reconCapture, reg PendingCloses) *Reconciler {
	bucket := newWeightBucket(1200, time.Minute, nil)
	rest := newBinanceREST(restConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Bucket: bucket})
	return newReconciler(ReconcilerConfig{
		REST: rest, Symbols: StaticSymbolMap{"BTC-USD": "BTCUSDT"},
		Closes: reg, CloseTimeout: 1500 * time.Millisecond,
		Pub: cap, Venue: "BINANCE", Tenant: "fund-alpha",
	})
}

// trackedFlatten is a POSITION close (an IOC market flatten): it leaves residual
// exposure if it does not land, so it carries a sweep side + leaves.
func trackedFlatten(reg *CloseRegistry, id string, age time.Duration) {
	reg.Track(CloseIntent{
		OrderID: id, InstrumentID: "BTC-USD",
		SweepSide: orderpb.Side_SIDE_SELL, Leaves: big.NewRat(1, 1),
		RequestedAt: time.Now().Add(-age).UTC(),
	})
}

func healedFrom(cap *reconCapture) *orderpb.StateHealed {
	var healed *orderpb.StateHealed
	for _, e := range cap.events {
		if h, ok := e.Payload.(*orderpb.StateHealed); ok {
			healed = h
		}
	}
	return healed
}

// A close the venue confirms terminal on query heals to the venue truth — no
// sweep, registry drained.
func TestBinanceHeal_ConfirmedTerminalNoSweep(t *testing.T) {
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","clientOrderId":"o1","status":"CANCELED","executedQty":"0"}`
	cap := &reconCapture{}
	reg := NewCloseRegistry()
	trackedFlatten(reg, "o1", 2*time.Second) // past the 1500ms timeout

	if err := healReconOverBinance(f, cap, reg).HealClosures(context.Background()); err != nil {
		t.Fatalf("HealClosures: %v", err)
	}
	if f.posts != 0 {
		t.Fatalf("posts = %d, want 0 (a confirmed-terminal close must not sweep)", f.posts)
	}
	if reg.Len() != 0 {
		t.Fatal("close not resolved after healing")
	}
	healed := healedFrom(cap)
	if healed == nil || healed.GetState().GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("want StateHealed CANCELLED (venue truth), got %+v", healed)
	}
}

// A close the venue confirms FILLED heals to that truth — the flatten landed, so
// there is nothing to sweep and the ledger learns the real fill.
func TestBinanceHeal_ConfirmedFilledAdoptsVenueTruth(t *testing.T) {
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","clientOrderId":"o1","status":"FILLED","executedQty":"1.00000000"}`
	cap := &reconCapture{}
	reg := NewCloseRegistry()
	trackedFlatten(reg, "o1", 2*time.Second)

	if err := healReconOverBinance(f, cap, reg).HealClosures(context.Background()); err != nil {
		t.Fatalf("HealClosures: %v", err)
	}
	if f.posts != 0 {
		t.Fatalf("posts = %d, want 0 (a filled close is not swept — that would double-flatten)", f.posts)
	}
	healed := healedFrom(cap)
	if healed == nil || healed.GetState().GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("want StateHealed FILLED (venue truth), got %+v", healed)
	}
}

// A close the venue still reports working (stuck) is force-cleared and its
// residual exposure swept via an aggressive market order under an idempotent
// heal- clOrdId; StateHealed → CANCELLED.
func TestBinanceHeal_StuckForceClearsAndSweeps(t *testing.T) {
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","clientOrderId":"o1","status":"NEW","executedQty":"0"}`
	f.newOrderBody = `{"symbol":"BTCUSDT","orderId":9,"clientOrderId":"heal-o1","status":"FILLED","executedQty":"1.00000000"}`
	f.accountBody = `{"balances":[]}`
	cap := &reconCapture{}
	reg := NewCloseRegistry()
	trackedFlatten(reg, "o1", 2*time.Second)

	if err := healReconOverBinance(f, cap, reg).HealClosures(context.Background()); err != nil {
		t.Fatalf("HealClosures: %v", err)
	}
	if f.posts != 1 {
		t.Fatalf("posts = %d, want 1 (a sweep market order)", f.posts)
	}
	if f.sawClientOrderID != "heal-o1" {
		t.Fatalf("sweep clOrdId = %q, want heal-o1 (idempotent — never double-flattens)", f.sawClientOrderID)
	}
	if f.sawType != "MARKET" {
		t.Fatalf("sweep type = %q, want MARKET (aggressive)", f.sawType)
	}
	if reg.Len() != 0 {
		t.Fatal("stuck close not resolved")
	}
	healed := healedFrom(cap)
	if healed == nil || healed.GetState().GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatal("stuck close must force-clear to CANCELLED")
	}
}

// An unresponsive venue (query returns an error body) still force-clears + sweeps
// — the ledger must never freeze on a venue that will not answer.
func TestBinanceHeal_UnresponsiveVenueStillClears(t *testing.T) {
	f := newFakeBinance(t)
	f.queryBody = `{"code":-1001,"msg":"internal error"}` // queryOrder → APIError
	f.newOrderBody = `{"symbol":"BTCUSDT","orderId":9,"clientOrderId":"heal-o1","status":"FILLED","executedQty":"1.00000000"}`
	f.accountBody = `{"balances":[]}`
	cap := &reconCapture{}
	reg := NewCloseRegistry()
	trackedFlatten(reg, "o1", 2*time.Second)

	if err := healReconOverBinance(f, cap, reg).HealClosures(context.Background()); err != nil {
		t.Fatalf("HealClosures: %v", err)
	}
	if f.posts != 1 {
		t.Fatalf("posts = %d, want 1 (sweep despite an unresponsive query)", f.posts)
	}
	if reg.Len() != 0 {
		t.Fatal("close must resolve even when the venue is unresponsive")
	}
}

// A cancelled RESTING order carries no residual exposure (it never traded), so a
// stuck cancel force-clears WITHOUT sweeping — sweeping would open a brand-new
// position in the opposite direction out of thin air.
func TestBinanceHeal_CancelledRestingOrderNeverSweeps(t *testing.T) {
	f := newFakeBinance(t)
	f.queryBody = `{"symbol":"BTCUSDT","clientOrderId":"o1","status":"NEW","executedQty":"0"}` // stuck
	f.accountBody = `{"balances":[]}`
	cap := &reconCapture{}
	reg := NewCloseRegistry()
	// The shape the OMS cancel path Tracks: no SweepSide, no Leaves.
	reg.Track(CloseIntent{
		OrderID: "o1", InstrumentID: "BTC-USD",
		RequestedAt: time.Now().Add(-2 * time.Second).UTC(),
	})

	if err := healReconOverBinance(f, cap, reg).HealClosures(context.Background()); err != nil {
		t.Fatalf("HealClosures: %v", err)
	}
	if f.posts != 0 {
		t.Fatalf("posts = %d, want 0 — a cancelled resting order must NEVER be swept", f.posts)
	}
	healed := healedFrom(cap)
	if healed == nil || healed.GetState().GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatal("a stuck cancel must still force-clear to CANCELLED")
	}
	if reg.Len() != 0 {
		t.Fatal("close not resolved")
	}
}

// A close still within the timeout window is left alone.
func TestBinanceHeal_NotYetDueLeftAlone(t *testing.T) {
	f := newFakeBinance(t)
	cap := &reconCapture{}
	reg := NewCloseRegistry()
	trackedFlatten(reg, "o1", 500*time.Millisecond) // < 1500ms

	if err := healReconOverBinance(f, cap, reg).HealClosures(context.Background()); err != nil {
		t.Fatalf("HealClosures: %v", err)
	}
	if f.posts != 0 || len(cap.events) != 0 {
		t.Fatalf("a not-yet-due close was acted on: posts=%d events=%d", f.posts, len(cap.events))
	}
	if reg.Len() != 1 {
		t.Fatal("a not-yet-due close must remain tracked")
	}
}

// --- the venue-close dispatch half: BinanceVenue satisfies Closer ---

// A cancel reaches the exchange addressed by our deterministic clOrdId.
func TestBinanceVenue_CancelOrderDispatches(t *testing.T) {
	f := newFakeBinance(t)
	f.cancelBody = `{"symbol":"BTCUSDT","origClientOrderId":"o1","status":"CANCELED"}`
	v := venueOverFake(f)

	err := v.CancelOrder(context.Background(), &orderpb.OrderState{OrderId: "o1", InstrumentId: "BTC-USD"})
	if err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if f.deletes != 1 {
		t.Fatalf("deletes = %d, want 1 (the cancel must reach the venue)", f.deletes)
	}
	if f.sawDeleteCl != "o1" {
		t.Fatalf("cancel origClientOrderId = %q, want o1 (our deterministic id)", f.sawDeleteCl)
	}
}

// -2011 "Unknown order sent" means the order is not working at the venue — the
// close has already landed, so the cancel is CONFIRMED, not failed.
func TestBinanceVenue_CancelUnknownOrderIsConfirmed(t *testing.T) {
	f := newFakeBinance(t)
	f.cancelBody = `{"code":-2011,"msg":"Unknown order sent."}`
	v := venueOverFake(f)

	if err := v.CancelOrder(context.Background(), &orderpb.OrderState{OrderId: "o1", InstrumentId: "BTC-USD"}); err != nil {
		t.Fatalf("CancelOrder = %v, want nil (-2011 ⇒ already gone ⇒ confirmed)", err)
	}
}

// Any other venue error leaves the close UNCONFIRMED — it must surface so the OMS
// leaves the intent tracked for the healing watchdog rather than assuming success.
func TestBinanceVenue_CancelErrorSurfaces(t *testing.T) {
	f := newFakeBinance(t)
	f.cancelBody = `{"code":-1001,"msg":"internal error"}`
	v := venueOverFake(f)

	if err := v.CancelOrder(context.Background(), &orderpb.OrderState{OrderId: "o1", InstrumentId: "BTC-USD"}); err == nil {
		t.Fatal("CancelOrder = nil, want an error — an unconfirmed cancel must never look like a success")
	}
}
