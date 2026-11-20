package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/testidentity"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
	"github.com/eighred/kanz/internal/venueadapter/server"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/test/backing"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
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
	domain := spiffeid.RequireTrustDomainFromString("kanz.internal")
	ca, err := testidentity.NewAuthority(domain)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(name string) (*testidentity.Source, spiffeid.ID) {
		id := spiffeid.RequireFromString("spiffe://kanz.internal/ns/kanz-services/sa/" + name)
		source, err := ca.Issue(id, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return source, id
	}
	oms, omsID := issue("oms")
	adapter, adapterID := issue("venue-binance")
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rpc := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsconfig.MTLSServerConfig(adapter, adapter, tlsconfig.AuthorizeID(omsID)))))
	venuepb.RegisterVenueAdapterServiceServer(rpc, server.NewWithDispatchEvidence(connector.Venue(), view, view,
		proof, halt.OpenGate(nil), slog.New(slog.NewTextHandler(io.Discard, nil))))
	go func() { _ = rpc.Serve(lis) }()
	cc, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(tlsconfig.MTLSClientConfig(oms, oms, tlsconfig.AuthorizeID(adapterID)))))
	if err != nil {
		rpc.Stop()
		t.Fatal(err)
	}
	return execution.NewGRPCVenue("BINANCE", "fixture-account", cc, "fund-alpha"), func() { _ = cc.Close(); rpc.Stop() }
}

func requireSignedFixtureRequests(t *testing.T, f *fakeBinance) {
	t.Helper()
	f.onRequest = func(r *http.Request) {
		params := r.URL.Query()
		signature := params.Get("signature")
		params.Del("signature")
		mac := hmac.New(sha256.New, []byte("fixture-secret"))
		_, _ = mac.Write([]byte(params.Encode()))
		if r.Header.Get("X-MBX-APIKEY") != "fixture-key" || signature != hex.EncodeToString(mac.Sum(nil)) {
			t.Error("unsigned synthetic account, placement or history request")
		}
	}
}

func TestMissingOrderAfterAdapterRestartCannotAuthorizeASecondPlacement(t *testing.T) {
	open := backing.VenueStore(t, "../../migrations")
	f := newFakeBinance(t)
	requireSignedFixtureRequests(t, f)
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
	ack, found, err := open().PlacementFor(ctx, order.GetOrderId())
	if err != nil || found || ack != (orderview.PlacementReceipt{}) {
		t.Fatalf("unknown POST acquired a durable acknowledgement: %+v %v %v", ack, found, err)
	}
	// A different order never reached the adapter. Both independent reads agree.
	view, err = restarted.QueryOrder(ctx, omsRedispatch("never-dispatched"))
	if err != nil || view.State != execution.OrderViewUnknown {
		t.Fatalf("venue miss before dispatch: %+v %v", view, err)
	}
	f.queryStatus, f.queryBody = http.StatusForbidden, `{"code":-2015,"msg":"Permission denied"}`
	if _, err := restarted.QueryOrder(ctx, order); err == nil {
		t.Fatal("denied recovery call became an affirmative answer")
	}
	f.queryStatus, f.queryBody = http.StatusOK, `{"symbol":"BTCUSDT","clientOrderId":"sent","orderId":7,"status":"PENDING_NEW","executedQty":"0"}`
	view, err = restarted.QueryOrder(ctx, order)
	if err != nil || view.State != execution.OrderViewIndeterminate {
		t.Fatalf("pending venue response affirmed placement: %+v %v", view, err)
	}
	if _, found, err := open().PlacementFor(ctx, order.GetOrderId()); err != nil || found {
		t.Fatal("unconfirmed recovery acquired a durable acknowledgement", err)
	}
	// Positive venue evidence can still recover the original order as resting.
	f.queryStatus, f.queryBody = http.StatusOK, `{"symbol":"BTCUSDT","clientOrderId":"sent","orderId":7,"status":"NEW","executedQty":"0"}`
	view, err = restarted.QueryOrder(ctx, order)
	if err != nil || view.State != execution.OrderViewWorking || len(view.Fills) != 0 || f.posts != 1 || f.myTradesCalls != 0 {
		t.Fatalf("resting order recovery: %+v %v placements=%d", view, err, f.posts)
	}
	receipt, found, err := open().PlacementFor(ctx, order.GetOrderId())
	want := orderview.Placement{AccountUID: "12345678", Symbol: "BTCUSDT", OrderID: 7}
	if err != nil || !found || receipt.Placement != want || receipt.Source != orderview.RecoveryQuery || receipt.RecordedAt.IsZero() {
		t.Fatalf("standalone positive recovery did not retain the original exchange identity: %+v %v", receipt, err)
	}
	stop()
	recovered, closeRecovered := start()
	defer closeRecovered()
	f.queryBody = `{"symbol":"BTCUSDT","clientOrderId":"sent","orderId":8,"status":"PARTIALLY_FILLED","executedQty":"0.6"}`
	view, err = recovered.QueryOrder(ctx, order)
	if err != nil || view.State != execution.OrderViewIndeterminate || len(view.Fills) != 0 || f.myTradesCalls != 0 || f.posts != 1 {
		t.Fatalf("changed exchange order reached trade history after recovery: %+v %v", view, err)
	}
	if retained, ok, err := open().PlacementFor(ctx, order.GetOrderId()); err != nil || !ok || retained != receipt {
		t.Fatalf("conflict changed the first recovery receipt: %+v %v", retained, err)
	}

}

func TestPendingBinanceTradeHistoryAcrossAdapterRestart(t *testing.T) {
	open := backing.VenueStore(t, "../../migrations")
	f := newFakeBinance(t)
	f.accountBody = `{"uid":12345678}`
	order := omsRedispatch("pending-history")
	const partial = `{"symbol":"BTCUSDT","orderId":7,"clientOrderId":"pending-history","status":"PARTIALLY_FILLED","executedQty":"0.6"}`
	f.newOrderBody, f.queryBody, f.myTradesBody = partial, partial, "[]"
	witness := open()
	want := orderview.Placement{AccountUID: "12345678", Symbol: "BTCUSDT", OrderID: 7}
	requireSignedFixtureRequests(t, f)
	f.onHistory = func() {
		got, found, err := witness.PlacementFor(t.Context(), order.GetOrderId())
		if err != nil || !found || got.Placement != want || got.Source != orderview.PlacementResponse || got.RecordedAt.IsZero() {
			t.Errorf("history started before committed placement: got=%+v found=%v err=%v", got, found, err)
		}
	}
	first, stop := startBinanceRPC(t, open, f)
	fills, err := first.Execute(t.Context(), order)
	stop()
	if status.Code(err) != codes.Unavailable || len(fills) != 0 || f.posts != 1 || f.myTradesCalls != 1 {
		t.Fatalf("pending history became a successful placement: fills=%d err=%v posts=%d reads=%d", len(fills), err, f.posts, f.myTradesCalls)
	}
	restarted, stop := startBinanceRPC(t, open, f)
	defer stop()
	// A new server and SQL pool must refuse another numeric exchange order
	// even though this positive signed answer echoes our original client ID.
	f.queryBody = strings.Replace(partial, `"orderId":7`, `"orderId":8`, 1)
	got, err := restarted.QueryOrder(t.Context(), order)
	if err != nil || got.State != execution.OrderViewIndeterminate || got.Reason == "" || len(got.Fills) != 0 || f.myTradesCalls != 1 {
		t.Fatalf("conflicting exchange identity reached history: %+v %v reads=%d", got, err, f.myTradesCalls)
	}
	f.queryBody = partial
	foreign := proto.Clone(order).(*orderpb.OrderState)
	foreign.PortfolioId = "other-portfolio"
	previousGets := f.gets
	got, err = restarted.QueryOrder(t.Context(), foreign)
	if err != nil || got.State != execution.OrderViewIndeterminate || len(got.Fills) != 0 || f.gets != previousGets {
		t.Fatalf("foreign portfolio reached persisted order: %+v %v", got, err)
	}
	for _, trades := range []string{"[]", `[{"symbol":"BTCUSDT","id":10,"orderId":7,"qty":"0.2","price":"50000","commission":"0.01","commissionAsset":"USD","isBuyer":true,"time":1750000000100}]`} {
		f.myTradesBody = trades
		view, err := restarted.QueryOrder(t.Context(), order)
		if err != nil || view.State != execution.OrderViewIndeterminate || len(view.Fills) != 0 || view.Reason == "" {
			t.Fatalf("incomplete signed history: %+v %v", view, err)
		}
	}
	// A signed positive answer carrying a valid client ID proves no local
	// admission when this tenant never recorded that order.
	stray := omsRedispatch("unrecorded")
	f.queryBody = strings.ReplaceAll(partial, "pending-history", "unrecorded")
	f.queryBody = strings.Replace(f.queryBody, `"orderId":7`, `"orderId":9`, 1)
	historyBefore := f.myTradesCalls
	got, err = restarted.QueryOrder(t.Context(), stray)
	if err != nil || got.State != execution.OrderViewIndeterminate || len(got.Fills) != 0 || f.myTradesCalls != historyBefore {
		t.Fatalf("unrecorded dispatch acquired executions: %+v %v", got, err)
	}
	f.queryBody = partial
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

// A second local client ID must not acquire the first order's exchange identity.
func TestAcknowledgementConflictStopsBeforeTradeHistory(t *testing.T) {
	open := backing.VenueStore(t, "../../migrations")
	f := newFakeBinance(t)
	f.accountBody = `{"uid":12345678}`
	rpc, stop := startBinanceRPC(t, open, f)
	defer stop()
	first := omsRedispatch("first")
	f.newOrderBody = `{"symbol":"BTCUSDT","orderId":7,"clientOrderId":"first","status":"NEW","executedQty":"0"}`
	if _, err := rpc.Execute(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	other := omsRedispatch("second")
	f.newOrderBody = `{"symbol":"BTCUSDT","orderId":7,"clientOrderId":"second","status":"PARTIALLY_FILLED","executedQty":"0.6"}`
	fills, err := rpc.Execute(t.Context(), other)
	if err == nil || len(fills) != 0 || f.posts != 2 || f.myTradesCalls != 0 {
		t.Fatalf("conflicting response reached trade history: fills=%d posts=%d history=%d err=%v", len(fills), f.posts, f.myTradesCalls, err)
	}
	store := open()
	if _, found, err := store.PlacementFor(t.Context(), other.GetOrderId()); found || err != nil {
		t.Fatalf("conflicting exchange identity was recorded: %v %v", found, err)
	}
	f.queryBody = f.newOrderBody
	v, err := rpc.QueryOrder(t.Context(), other)
	if err != nil || v.State != execution.OrderViewIndeterminate || len(v.Fills) != 0 || f.myTradesCalls != 0 {
		t.Fatalf("unsigned alias adopted another order on query: %+v %v", v, err)
	}
	if _, err = rpc.Execute(t.Context(), other); status.Code(err) != codes.FailedPrecondition || f.posts != 2 {
		t.Fatalf("refused acknowledgement authorized another placement: %v", err)
	}
}

func TestBinanceStartupRequiresAcknowledgementMigration(t *testing.T) {
	old := t.TempDir()
	for _, migration := range []string{"0001_venue_orders.sql", "0002_tenant_scope_required.sql", "0003_pending_closes.sql"} {
		b, err := os.ReadFile(filepath.Join("../../migrations", migration))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(old, migration), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	view := backing.VenueStore(t, old)()
	f := newFakeBinance(t)
	f.accountBody = `{"uid":12345678}`
	conn := NewBinanceConnector(VenueSettings{MIC: "BINANCE", Account: "fixture-account", BaseURL: f.srv.URL, APIKey: "fixture-key", APISecret: "fixture-secret", Symbols: map[string]string{"BTC-USD": "BTCUSDT"}}, "", view)
	proof, err := conn.Start(t.Context(), "12345678", WorkerDeps{})
	if err == nil || proof.Verified || conn.venue.provenAccount() != "" || f.posts != 0 {
		t.Fatalf("old schema admitted the connector: proof=%t placements=%d err=%v", proof.Verified, f.posts, err)
	}
}
