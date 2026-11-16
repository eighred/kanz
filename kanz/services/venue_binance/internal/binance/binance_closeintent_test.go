package binance

// THE IN-FLIGHT CERTAINTY SEAM, DRIVEN FROM THE RPC THAT ACTUALLY WRITES IT (#1036).
//
// Every other healing test in this package hands the registry an intent it built
// by hand, with InstrumentID already filled in. That proves the watchdog works on
// an intent NOTHING IN PRODUCTION PRODUCES: in the out-of-process deployment —
// the only one that reaches a real exchange — the sole writer of a tracked close
// is venueadapter/server.CancelOrder, and it recorded the order id alone. The
// watchdog's first act is to map InstrumentID to a venue symbol, an empty id maps
// to nothing, and every close was therefore dropped WITHOUT ASKING THE EXCHANGE
// ANYTHING.
//
// So this test assembles the real parts — the real gRPC shell, the real
// BinanceVenue over an HTTP server that returns a 504 on the cancel, and the real
// reconciler over the same registry — and measures the two things that cost money:
// what the tracked intent carries, and whether a query-order call reached the
// exchange.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
	"github.com/eighred/kanz/internal/venueadapter/server"
)

// adapterOverFake is the out-of-process deployment in miniature: the venue-agnostic
// gRPC shell fronting the real Binance connector, sharing ONE close registry with
// the reconciler — exactly how venue-binance's composition root wires them.
func adapterOverFake(t *testing.T, f *fakeBinance, closes *CloseRegistry) *server.Server {
	t.Helper()
	return server.New(venueOverFake(f), orderview.NewMemory(), closes,
		execution.AccountProof{Verified: true, ExchangeAccountID: "12345678"},
		halt.OpenGate(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// healReconOverFake is the watchdog half, with a clock already past the close
// timeout so one HealClosures pass sees the intent as due.
func healReconOverFake(f *fakeBinance, cap *reconCapture, reg PendingCloses) *Reconciler {
	bucket := newWeightBucket(1200, time.Minute, nil)
	rest := newBinanceREST(restConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Bucket: bucket})
	return newReconciler(ReconcilerConfig{
		REST: rest, Symbols: StaticSymbolMap{"BTC-USD": "BTCUSDT"},
		Closes: reg, CloseTimeout: 1500 * time.Millisecond,
		Pub: cap, Venue: "BINANCE", Tenant: "fund-alpha",
		Now: func() time.Time { return time.Now().Add(5 * time.Second) },
	})
}

// omsCancelling is the OMS's own record of a resting order it wants withdrawn —
// what a CancelOrderRequest actually carries on the wire.
func omsCancelling(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: id, InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType:  orderpb.OrderType_ORDER_TYPE_LIMIT,
		Status:     orderpb.OrderStatus_ORDER_STATUS_ROUTED,
		LimitPrice: bdec("50000"), OrderedQuantity: bdec("1"),
	}
}

func TestBinanceCloseIntent_AnAmbiguousCancelIsQueriedAtTheVenueNotDropped(t *testing.T) {
	ctx := context.Background()
	f := newFakeBinance(t)
	// The exchange does not answer the withdrawal. This is the ONLY case the
	// healing seam exists for: the order may still be resting and fillable.
	f.cancelStatus = http.StatusGatewayTimeout
	// What the exchange says when the watchdog finally asks: still working. The
	// fund is holding a live order the OMS has already booked as CANCELLED.
	f.queryBody = `{"symbol":"BTCUSDT","clientOrderId":"o1","status":"NEW","executedQty":"0"}`

	closes := NewCloseRegistry()
	srv := adapterOverFake(t, f, closes)

	if _, err := srv.CancelOrder(ctx, &venuepb.CancelOrderRequest{State: omsCancelling("o1")}); err == nil {
		t.Fatal("CancelOrder reported a CONFIRMED withdrawal for a cancel the exchange never answered")
	}
	if f.deletes != 1 {
		t.Fatalf("the adapter issued %d cancels at the exchange, want 1", f.deletes)
	}

	// 1. THE INTENT MUST CARRY THE INSTRUMENT. Without it the watchdog cannot map
	//    a venue symbol and cannot form the query at all.
	due, err := closes.DueCloses(context.Background(), time.Now().Add(2*time.Second), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("the registry holds %d in-flight closes after an unanswered cancel, want 1 — "+
			"the watchdog has nothing to heal and the order stays live at the exchange", len(due))
	}
	if got := due[0].InstrumentID; got != "BTC-USD" {
		t.Fatalf("the close tracked through CancelOrder carries instrument_id %q, want %q. "+
			"The watchdog opens by mapping this field to a venue symbol and drops the intent when "+
			"it does not resolve, so an empty id means EVERY ambiguous cancel in the out-of-process "+
			"deployment is discarded without one question asked of the exchange (#1036)", got, "BTC-USD")
	}

	before := f.gets
	cap := &reconCapture{}
	if err := healReconOverFake(f, cap, closes).HealClosures(ctx); err == nil {
		t.Fatal("working close must remain unconfirmed")
	}
	if f.gets != before+1 || f.posts != 0 || len(cap.events) != 0 || closes.Len() != 1 {
		t.Fatalf("close ownership lost: gets=%d posts=%d events=%d pending=%d", f.gets-before, f.posts, len(cap.events), closes.Len())
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
func TestBinanceCloseIntent_AnUnhealableCloseIsReportedNotSilentlyResolved(t *testing.T) {
	for _, tc := range []struct {
		name       string
		instrument string
		want       string
	}{
		{"no instrument at all", "", execution.CloseDropNoInstrument},
		{"an instrument this venue has no symbol for", "ETH-USD", execution.CloseDropUnmappedSymbol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeBinance(t)
			reg := NewCloseRegistry()
			_ = reg.Track(context.Background(), CloseIntent{
				OrderID: "o1", InstrumentID: tc.instrument,
				RequestedAt: time.Now().Add(-2 * time.Second).UTC(),
			})

			var reasons []string
			bucket := newWeightBucket(1200, time.Minute, nil)
			rest := newBinanceREST(restConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Bucket: bucket})
			rec := newReconciler(ReconcilerConfig{
				REST: rest, Symbols: StaticSymbolMap{"BTC-USD": "BTCUSDT"},
				Closes: reg, CloseTimeout: 1500 * time.Millisecond,
				Pub: &reconCapture{}, Venue: "BINANCE", Tenant: "fund-alpha",
				OnCloseUnhealable: func(_, _, reason string) { reasons = append(reasons, reason) },
			})

			if err := rec.HealClosures(context.Background()); err == nil {
				t.Fatal("unqueryable close must report incomplete pass")
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
