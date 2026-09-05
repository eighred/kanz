package okx

// THE IN-FLIGHT CERTAINTY SEAM, DRIVEN FROM THE RPC THAT ACTUALLY WRITES IT (#1036).
//
// The OKX half of the same proof as binance_closeintent_test.go. Every other
// healing test here hands the registry a CloseIntent it built by hand with
// InstrumentID already set — an intent nothing in production produces. In the
// out-of-process deployment the ONLY writer is venueadapter/server.CancelOrder,
// and it recorded the order id alone, so the watchdog's opening symbol lookup
// missed and every ambiguous cancel was discarded unqueried.
//
// Assembled from the real parts: the real gRPC shell over the real OKXVenue,
// pointed at an HTTP server with no cancel endpoint (the withdrawal never gets an
// answer), sharing one registry with the real reconciler.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
	"github.com/eighred/kanz/internal/venueadapter/server"
)

// okxAdapterOver is the out-of-process deployment in miniature: the venue-agnostic
// gRPC shell fronting the real OKX connector, sharing ONE close registry with the
// reconciler — exactly how venue-okx's composition root wires them.
func okxAdapterOver(t *testing.T, f *fakeOKX, closes *CloseRegistry) *server.Server {
	t.Helper()
	return server.New(okxVenueOver(f), orderview.NewMemory(), closes,
		execution.AccountProof{Verified: true, ExchangeAccountID: "9999"},
		halt.OpenGate(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// okxHealReconOver is the watchdog half, on a clock already past the close
// timeout so a single HealClosures pass sees the intent as due.
func okxHealReconOver(f *fakeOKX, cap *okxCapture, reg PendingCloses) *OKXReconciler {
	bucket := NewWeightBucket(60, time.Minute, nil)
	rest := newOKXREST(okxRestConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s",
		Passphrase: "p", Buckets: newOKXBuckets(bucket, nil), Mode: exchangeauth.OKXDemo})
	return newOKXReconciler(OKXReconcilerConfig{
		REST: rest, Symbols: StaticSymbolMap{"BTC-USD": "BTC-USDT"},
		Closes: reg, CloseTimeout: 1500 * time.Millisecond,
		Pub: cap, Venue: "OKX", Tenant: "fund-alpha",
		Now: func() time.Time { return time.Now().Add(5 * time.Second) },
	})
}

// okxCancelling is the OMS's own record of a resting order it wants withdrawn —
// what a CancelOrderRequest carries on the wire.
func okxCancelling(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: id, InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType:  orderpb.OrderType_ORDER_TYPE_LIMIT,
		Status:     orderpb.OrderStatus_ORDER_STATUS_ROUTED,
		LimitPrice: odec("50000"), OrderedQuantity: odec("1"),
	}
}

func TestOKXCloseIntent_AnAmbiguousCancelIsQueriedAtTheVenueNotDropped(t *testing.T) {
	ctx := context.Background()
	f := newFakeOKX(t)
	// The exchange still reports the order LIVE when the watchdog finally asks:
	// the withdrawal never landed, and the OMS has already booked it CANCELLED.
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"live","accFillSz":"0"}]}`

	closes := NewCloseRegistry()
	srv := okxAdapterOver(t, f, closes)

	if _, err := srv.CancelOrder(ctx, &venuepb.CancelOrderRequest{State: okxCancelling("o1")}); err == nil {
		t.Fatal("CancelOrder reported a CONFIRMED withdrawal for a cancel the exchange never answered")
	}

	// 1. THE INTENT MUST CARRY THE INSTRUMENT. Without it the watchdog cannot map
	//    a venue instId and cannot form the query at all.
	due := closes.DueCloses(time.Now().Add(time.Hour), 0)
	if len(due) != 1 {
		t.Fatalf("the registry holds %d in-flight closes after an unanswered cancel, want 1 — "+
			"the watchdog has nothing to heal and the order stays live at the exchange", len(due))
	}
	if got := due[0].InstrumentID; got != "BTC-USD" {
		t.Fatalf("the close tracked through CancelOrder carries instrument_id %q, want %q. "+
			"The watchdog opens by mapping this field to a venue instId and drops the intent when "+
			"it does not resolve, so an empty id means EVERY ambiguous cancel in the out-of-process "+
			"deployment is discarded without one question asked of the exchange (#1036)", got, "BTC-USD")
	}

	// 2. AND THE WATCHDOG MUST ACTUALLY ASK. Counted at the exchange.
	before := f.gets
	cap := &okxCapture{}
	if err := okxHealReconOver(f, cap, closes).HealClosures(ctx); err != nil {
		t.Fatalf("HealClosures: %v", err)
	}
	if f.gets != before+1 {
		t.Fatalf("the healing watchdog issued %d query-order calls for an unconfirmed close, want 1. "+
			"It resolved the intent without asking the exchange anything: no StateHealed, no "+
			"force-clear, no balance re-anchor — and the OMS has already recorded the order CANCELLED",
			f.gets-before)
	}
	if closes.Len() != 0 {
		t.Fatalf("the registry still holds %d closes after healing — the watchdog would re-heal it forever", closes.Len())
	}
	// A cancelled resting order carries no residual exposure: force-clear, never sweep.
	if f.posts != 0 {
		t.Fatalf("the watchdog placed %d orders healing a CANCEL — a withdrawn resting order has "+
			"not traded, so sweeping it opens a brand-new position out of nothing", f.posts)
	}
	var healed *orderpb.StateHealed
	for _, e := range cap.events {
		if h, ok := e.Payload.(*orderpb.StateHealed); ok {
			healed = h
		}
	}
	if healed == nil {
		t.Fatal("no StateHealed FACT was emitted for a close the exchange still reports live — " +
			"nothing downstream ever learns the withdrawal did not land")
	}
	if got := healed.GetState().GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("StateHealed carries %v, want CANCELLED (force-cleared)", got)
	}
	if got := healed.GetState().GetInstrumentId(); got != "BTC-USD" {
		t.Fatalf("the correcting FACT names instrument %q, want BTC-USD — a StateHealed with no "+
			"instrument cannot be folded into a position book", got)
	}
}

// AN UNHEALABLE CLOSE IS REPORTED, NOT SILENTLY RESOLVED (#1036).
//
// Both ways a close can fail to become a venue question used to end on the same
// Resolve() line as a close healed against venue truth. That is what made the
// defect invisible for as long as it existed: every close the out-of-process
// adapter tracked carried an empty instrument, Symbol("") missed, and the miss
// read as the perfectly ordinary "this venue does not trade that". The two
// answers are different — one is a writer defect, one is a symbol map — and
// neither may look like a heal.
func TestOKXCloseIntent_AnUnhealableCloseIsReportedNotSilentlyResolved(t *testing.T) {
	for _, tc := range []struct {
		name       string
		instrument string
		want       string
	}{
		{"no instrument at all", "", execution.CloseDropNoInstrument},
		{"an instrument this venue has no symbol for", "ETH-USD", execution.CloseDropUnmappedSymbol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOKX(t)
			reg := NewCloseRegistry()
			reg.Track(CloseIntent{
				OrderID: "o1", InstrumentID: tc.instrument,
				RequestedAt: time.Now().Add(-2 * time.Second).UTC(),
			})

			var reasons []string
			bucket := NewWeightBucket(60, time.Minute, nil)
			rest := newOKXREST(okxRestConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s",
				Passphrase: "p", Buckets: newOKXBuckets(bucket, nil), Mode: exchangeauth.OKXDemo})
			rec := newOKXReconciler(OKXReconcilerConfig{
				REST: rest, Symbols: StaticSymbolMap{"BTC-USD": "BTC-USDT"},
				Closes: reg, CloseTimeout: 1500 * time.Millisecond,
				Pub: &okxCapture{}, Venue: "OKX", Tenant: "fund-alpha",
				OnCloseUnhealable: func(_, _, reason string) { reasons = append(reasons, reason) },
			})

			if err := rec.HealClosures(context.Background()); err != nil {
				t.Fatalf("HealClosures: %v", err)
			}
			if len(reasons) != 1 || reasons[0] != tc.want {
				t.Fatalf("the watchdog reported %v for a close it could not attempt, want exactly [%s]. "+
					"Dropping it silently makes a seam that has never reached the exchange "+
					"indistinguishable from one that finds nothing wrong", reasons, tc.want)
			}
			if f.gets != 0 {
				t.Fatalf("the watchdog issued %d query-order calls for an intent it cannot address — "+
					"it cannot have known which symbol to ask about", f.gets)
			}
		})
	}
}
