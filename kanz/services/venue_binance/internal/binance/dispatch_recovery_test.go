package binance

import (
	"context"
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
	"github.com/eighred/kanz/internal/venueadapter/server"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/test/backing"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestMissingOrderAfterAdapterRestartCannotAuthorizeASecondPlacement(t *testing.T) {
	open := backing.VenueStore(t, "../../migrations")
	f := newFakeBinance(t)
	ctx := t.Context()
	start := func() (*execution.GRPCVenue, func()) {
		view := open()
		connector := NewBinanceConnector(VenueSettings{MIC: "BINANCE", Account: "fixture",
			BaseURL: f.srv.URL, APIKey: "fixture-key", APISecret: "fixture-secret",
			Symbols: map[string]string{"BTC-USD": "BTCUSDT"}}, "", view)
		lis := bufconn.Listen(1 << 20)
		rpc := grpc.NewServer()
		venuepb.RegisterVenueAdapterServiceServer(rpc, server.NewWithDispatchEvidence(connector.Venue(), view, view,
			execution.AccountProof{Verified: true, ExchangeAccountID: "fixture"}, halt.OpenGate(nil), slog.New(slog.NewTextHandler(io.Discard, nil))))
		go func() { _ = rpc.Serve(lis) }()
		cc, err := grpc.NewClient("passthrough:///fixture", grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
		if err != nil {
			rpc.Stop()
			t.Fatal(err)
		}
		return execution.NewGRPCVenue("BINANCE", "fixture", cc, "fund-alpha"), func() { _ = cc.Close(); rpc.Stop() }
	}

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
