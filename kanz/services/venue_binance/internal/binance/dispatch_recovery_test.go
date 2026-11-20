package binance

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
	"github.com/eighred/kanz/internal/venueadapter/server"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/test/backing"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func startBinanceRPC(t *testing.T, open func() *orderview.Postgres, f *fakeBinance) (*execution.GRPCVenue, func()) {
	t.Helper()
	ctx := t.Context()
	view := open()
	connector := NewBinanceConnector(VenueSettings{MIC: "BINANCE", Account: "fixture-account",
		BaseURL: f.srv.URL, APIKey: "fixture-key", APISecret: "fixture-secret",
		Symbols: map[string]string{"BTC-USD": "BTCUSDT"}}, "", view)
	proof, err := connector.Start(ctx, "12345678", WorkerDeps{})
	if err != nil || !proof.Verified {
		t.Fatalf("signed loopback account proof: %v", err)
	}
	lis := bufconn.Listen(1 << 20)
	rpc := grpc.NewServer()
	venuepb.RegisterVenueAdapterServiceServer(rpc, server.NewWithDispatchEvidence(connector.Venue(), view, view,
		proof, halt.OpenGate(nil), slog.New(slog.NewTextHandler(io.Discard, nil))))
	go func() { _ = rpc.Serve(lis) }()
	cc, err := grpc.NewClient("passthrough:///fixture", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		rpc.Stop()
		t.Fatal(err)
	}
	return execution.NewGRPCVenue("BINANCE", "fixture-account", cc, "fund-alpha"), func() { _ = cc.Close(); rpc.Stop() }
}

func TestMissingOrderAfterAdapterRestartCannotAuthorizeASecondPlacement(t *testing.T) {
	open := backing.VenueStore(t, "../../migrations")
	f := newFakeBinance(t)
	f.accountBody = `{"uid":12345678}`
	ctx := t.Context()
	start := func() (*execution.GRPCVenue, func()) { return startBinanceRPC(t, open, f) }

	// The fake exchange observed the POST, but neither operation returned
	// a usable acknowledgement. The restarted server uses a fresh SQL pool.
	f.newOrderStatus, f.queryStatus = http.StatusGatewayTimeout, http.StatusServiceUnavailable
	first, stop := start()
	order := omsRedispatch("sent")
	// A second RPC arrives after the first placement has reached the exchange
	// but before the first caller receives its lost acknowledgement.
	posted, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	resume := func() { unblock.Do(func() { close(release) }) }
	defer resume()
	var sent atomic.Bool
	f.onPost = func() {
		if sent.CompareAndSwap(false, true) {
			close(posted)
			<-release
		}
	}
	placed := make(chan error, 1)
	go func() { _, err := first.Execute(ctx, order); placed <- err }()
	select {
	case <-posted:
	case <-time.After(5 * time.Second):
		t.Fatal("first placement did not reach the venue")
	}
	concurrent, closeConcurrent := start()
	timeout, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, duplicateErr := concurrent.Execute(timeout, order)
	closeConcurrent()
	resume()
	err := <-placed
	stop()
	if status.Code(duplicateErr) != codes.FailedPrecondition {
		t.Fatalf("concurrent placement returned %v", duplicateErr)
	}
	if err == nil || f.posts != 1 || f.gets != 1 {
		t.Fatalf("placement interruption: err=%v placements=%d lookups=%d", err, f.posts, f.gets)
	}
	restarted, stop := start()
	defer stop()
	f.queryStatus, f.queryBody = http.StatusBadRequest, `{"code":-2013,"msg":"Order does not exist."}`
	view, err := restarted.QueryOrder(ctx, order)
	if err != nil || view.State != execution.OrderViewIndeterminate || view.Reason == "" || len(view.Fills) != 0 {
		t.Fatalf("lost response followed by a venue miss: %+v %v", view, err)
	}
	if _, err := restarted.Execute(ctx, order); status.Code(err) != codes.FailedPrecondition || f.posts != 1 {
		t.Fatalf("repeated placement after restart: %v, placements=%d", err, f.posts)
	}
	// A different order never reached the adapter. Both independent reads agree.
	view, err = restarted.QueryOrder(ctx, omsRedispatch("never-dispatched"))
	if err != nil || view.State != execution.OrderViewUnknown {
		t.Fatalf("venue miss before dispatch: %+v %v", view, err)
	}
	// Positive venue evidence can still recover the original order as resting.
	f.queryStatus, f.queryBody = http.StatusOK, `{"symbol":"BTCUSDT","clientOrderId":"sent","orderId":7,"status":"NEW","executedQty":"0"}`
	view, err = restarted.QueryOrder(ctx, order)
	if err != nil || view.State != execution.OrderViewWorking || len(view.Fills) != 0 || f.posts != 1 || f.myTradesCalls != 0 {
		t.Fatalf("resting order recovery: %+v %v placements=%d", view, err, f.posts)
	}
}

func TestPendingBinanceTradeHistoryAcrossAdapterRestart(t *testing.T) {
	open := backing.VenueStore(t, "../../migrations")
	f := newFakeBinance(t)
	f.accountBody = `{"uid":12345678}`
	order := omsRedispatch("pending-history")
	const partial = `{"symbol":"BTCUSDT","orderId":7,"clientOrderId":"pending-history","status":"PARTIALLY_FILLED","executedQty":"0.6"}`
	f.newOrderBody, f.queryBody, f.myTradesBody = partial, partial, "[]"
	first, stop := startBinanceRPC(t, open, f)
	fills, err := first.Execute(t.Context(), order)
	stop()
	if status.Code(err) != codes.Unavailable || len(fills) != 0 || f.posts != 1 || f.myTradesCalls != 1 {
		t.Fatalf("pending history became a successful placement: fills=%d err=%v posts=%d reads=%d", len(fills), err, f.posts, f.myTradesCalls)
	}
	restarted, stop := startBinanceRPC(t, open, f)
	defer stop()
	for _, trades := range []string{"[]", `[{"symbol":"BTCUSDT","id":10,"orderId":7,"qty":"0.2","price":"50000","commission":"0.01","commissionAsset":"USD","isBuyer":true,"time":1750000000100}]`} {
		f.myTradesBody = trades
		view, err := restarted.QueryOrder(t.Context(), order)
		if err != nil || view.State != execution.OrderViewIndeterminate || len(view.Fills) != 0 || view.Reason == "" {
			t.Fatalf("incomplete signed history: %+v %v", view, err)
		}
	}
	// The exchange now serves the entire match. Its own per-match time
	// travels over the typed RPC, independent of either adapter's clock.
	f.myTradesBody = `[{"symbol":"BTCUSDT","id":10,"orderId":7,"qty":"0.6","price":"50000","commission":"0.01","commissionAsset":"USD","isBuyer":true,"time":1750000000100}]`
	view, err := restarted.QueryOrder(t.Context(), order)
	if err != nil || view.State != execution.OrderViewPartiallyFilled || len(view.Fills) != 1 || view.Fills[0].GetExecutedAt().AsTime().UnixMilli() != 1750000000100 {
		t.Fatalf("complete signed history did not recover the original match: %+v %v", view, err)
	}
	if _, err := restarted.Execute(t.Context(), order); status.Code(err) != codes.FailedPrecondition || f.posts != 1 {
		t.Fatalf("placement after restart: %v, POSTs=%d", err, f.posts)
	}
}

func TestConfirmedBinanceRestingAcknowledgementDoesNotPermitReplacement(t *testing.T) {
	open := backing.VenueStore(t, "../../migrations")
	f := newFakeBinance(t)
	f.accountBody = `{"uid":12345678}`
	order := omsRedispatch("resting")
	const resting = `{"symbol":"BTCUSDT","orderId":8,"clientOrderId":"resting","status":"NEW","executedQty":"0"}`
	f.newOrderBody, f.queryBody = resting, resting
	first, stop := startBinanceRPC(t, open, f)
	fills, err := first.Execute(t.Context(), order)
	stop()
	if err != nil || len(fills) != 0 || f.posts != 1 || f.myTradesCalls != 0 {
		t.Fatalf("exact resting acknowledgement: fills=%d err=%v posts=%d trade reads=%d", len(fills), err, f.posts, f.myTradesCalls)
	}
	restarted, stop := startBinanceRPC(t, open, f)
	defer stop()
	view, err := restarted.QueryOrder(t.Context(), order)
	if err != nil || view.State != execution.OrderViewWorking || view.ExecutedQuantity == nil || view.ExecutedQuantity.GetCoefficient() != 0 {
		t.Fatalf("confirmed resting order after restart: %+v %v", view, err)
	}
	if _, err := restarted.Execute(t.Context(), order); status.Code(err) != codes.FailedPrecondition || f.posts != 1 {
		t.Fatalf("resting order placement after restart: err=%v placements=%d", err, f.posts)
	}
}

func TestBinancePlacementRequiresConfirmedRestingOrCompleteMatches(t *testing.T) {
	for _, tc := range []struct{ name, fields string }{
		{"new missing total", `"status":"NEW"`},
		{"new invalid total", `"status":"NEW","executedQty":"-1"`},
		{"new with traded quantity", `"status":"NEW","executedQty":"0.1"`},
		{"new with inline fills", `"status":"NEW","executedQty":"0","fills":[{"qty":"0.1"}]`},
		{"filled without trades", `"status":"FILLED","executedQty":"1"`},
		{"pending", `"status":"PENDING_NEW","executedQty":"0"`},
		{"cancelled", `"status":"CANCELED","executedQty":"0"`},
		{"expired", `"status":"EXPIRED","executedQty":"0"`},
		{"rejected", `"status":"REJECTED","executedQty":"0"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeBinance(t)
			f.newOrderBody = `{"symbol":"BTCUSDT","orderId":7,"clientOrderId":"check",` + tc.fields + `}`
			f.myTradesBody = "[]"
			fills, err := venueOverFake(f).Execute(t.Context(), omsRedispatch("check"))
			if !errors.Is(err, execution.ErrHistoryIncomplete) || len(fills) != 0 || f.posts != 1 {
				t.Fatalf("unresolved placement: err=%v fills=%d POSTs=%d", err, len(fills), f.posts)
			}
		})
	}
}
