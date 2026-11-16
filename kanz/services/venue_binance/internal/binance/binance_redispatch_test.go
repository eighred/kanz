package binance

// A RE-DISPATCH OF A FILLED ORDER REACHES NEITHER THE EXCHANGE NOR THE VIEW (#914).
//
// This is the end-to-end half, and it is deliberately assembled out of the real
// parts rather than the seam in isolation: the real user-data ingester parses a
// real Binance execution report, folds it through the real orderview.Memory, and
// the real gRPC shell is then asked to work that order again over the real
// BinanceVenue pointed at an HTTP server that counts placements.
//
// So the two assertions that matter are measured where they cost money — the
// number of POSTs to /api/v3/order, and what the reconciler does on its next two
// passes — rather than at a boundary a test double could answer for.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/venueadapter/server"
)

// omsRedispatch is the OMS's own record of the order, which is what a sweep, a
// resume or a broker redelivery hands the adapter: it has not folded the fill
// yet, so it still says the order is being routed and nothing has traded.
func omsRedispatch(id string) *orderpb.OrderState {
	st := kanzOrder(id)
	st.Status = orderpb.OrderStatus_ORDER_STATUS_ROUTED
	// The terms the connector needs to place a LIMIT order at all — kanzOrder
	// carries only what the fill path reads back out of the view.
	st.LimitPrice = bdec("50000")
	st.TimeInForce = orderpb.TimeInForce_TIME_IN_FORCE_GTC
	return st
}

func TestRedispatch_AFilledOrderIsNeitherReplacedNorReopened(t *testing.T) {
	ctx := context.Background()

	// 1. The venue fills the order, and the real ingester folds its own report
	//    into the adapter's real view — the #904 write.
	view := newFakeOrders("o1")
	_ = ingesterOverView([][]byte{[]byte(filledReport)}, &reconCapture{}, view).Run(ctx)
	if got := view.get(t, "o1").GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("setup: the view has o1 at %v, want FILLED", got)
	}

	// 2. The OMS re-dispatches it, through the real gRPC shell over the real
	//    connector.
	f := newFakeBinance(t)
	f.newOrderBody = fullFill("o1")
	srv := server.New(venueOverFake(f), view.store, execution.NewCloseRegistry(),
		execution.AccountProof{Verified: true, ExchangeAccountID: "12345678"},
		halt.OpenGate(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := srv.Execute(ctx, &venuepb.ExecuteRequest{State: omsRedispatch("o1")})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("re-dispatching a filled order returned %v, want AlreadyExists", err)
	}
	if f.posts != 0 {
		t.Fatalf("%d new-order placements reached the exchange for an order it had already "+
			"filled — that is the fund buying the same position twice, and no record downstream "+
			"would call it a duplicate: the second placement returns real fills under a client "+
			"order id the exchange is free to reuse once the first one is no longer open", f.posts)
	}

	// 3. And the view is exactly as the venue's report left it.
	if got := view.get(t, "o1").GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("the view has o1 at %v after the re-dispatch, want FILLED — the OMS's stale "+
			"status overwrote the venue's own verdict (#914)", got)
	}
	if open := view.openIDs(t); len(open) != 0 {
		t.Fatalf("the view believes %v are open at the venue after the re-dispatch — the leak "+
			"#904 closed has reopened one call away from it", open)
	}

	// 4. THE CONSEQUENCE, MEASURED AT THE EXCHANGE, exactly as #913 measures it:
	//    the reconciler must still spend no REST weight on this order and still
	//    emit no StateHealed about it.
	rcap := &reconCapture{}
	bucket := newWeightBucket(1200, time.Minute, nil)
	rest := newBinanceREST(restConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Bucket: bucket})
	r := newReconciler(ReconcilerConfig{
		REST: rest, Symbols: StaticSymbolMap{"BTC-USD": "BTCUSDT"},
		Expected: view, Pub: rcap, Venue: "BINANCE", Tenant: "fund-alpha",
	})
	for pass := range 2 {
		if err := r.Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile pass %d: %v", pass, err)
		}
	}
	if f.gets != 0 {
		t.Fatalf("the reconciler issued %d query-order calls after the re-dispatch — the finished "+
			"order is back on the rate-limited weight budget every pass", f.gets)
	}
	for _, e := range rcap.events {
		if _, ok := e.Payload.(*orderpb.StateHealed); ok {
			t.Fatal("a StateHealed FACT was emitted for an order that already filled — the event " +
				"spine repeats itself about a finished order, forever")
		}
	}
}

// THE NON-REGRESSION, END TO END. A partial fill leaves the order LIVE, and a
// re-dispatch of a live order must still reach the exchange. If this stops
// posting, the guard has turned every recovery path into a trading outage.
func TestRedispatch_APartiallyFilledOrderStillReachesTheExchange(t *testing.T) {
	ctx := context.Background()

	view := newFakeOrders("o1")
	_ = ingesterOverView([][]byte{[]byte(partialReport)}, &reconCapture{}, view).Run(ctx)
	if got := view.get(t, "o1").GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		t.Fatalf("setup: the view has o1 at %v, want PARTIALLY_FILLED", got)
	}

	f := newFakeBinance(t)
	f.newOrderBody = fullFill("o1")
	srv := server.New(venueOverFake(f), view.store, execution.NewCloseRegistry(),
		execution.AccountProof{Verified: true, ExchangeAccountID: "12345678"},
		halt.OpenGate(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := srv.Execute(ctx, &venuepb.ExecuteRequest{State: omsRedispatch("o1")}); err != nil {
		t.Fatalf("re-dispatching a partially filled order was refused: %v — the order is still "+
			"live at the exchange and re-working it is how an interrupted one recovers", err)
	}
	if f.posts != 1 {
		t.Fatalf("the exchange saw %d placements for a live order, want 1", f.posts)
	}
}
