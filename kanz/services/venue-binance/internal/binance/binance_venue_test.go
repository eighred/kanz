package binance

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

// fakeBinance is a minimal stand-in for the Binance Spot REST surface — the
// hermetic test seam (no network, no keys). It records the placement params and
// serves scripted new-order and query-order responses.
type fakeBinance struct {
	srv *httptest.Server

	sawClientOrderID string
	sawSignature     bool
	sawAPIKey        bool
	sawType          string

	posts       int    // POST /api/v3/order count — a healing sweep is a POST
	deletes     int    // DELETE /api/v3/order count — a venue cancel
	sawDeleteCl string // origClientOrderId of the last cancel

	newOrderStatus int    // HTTP status for POST /api/v3/order
	newOrderBody   string // body for POST
	queryBody      string // body for GET /api/v3/order
	cancelBody     string // body for DELETE /api/v3/order
	accountBody    string // body for GET /api/v3/account
	tickerPrice    string // price for GET /api/v3/ticker/price
}

func newFakeBinance(t *testing.T) *fakeBinance {
	t.Helper()
	f := &fakeBinance{newOrderStatus: 200}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/order", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.sawSignature = q.Get("signature") != ""
		f.sawAPIKey = r.Header.Get("X-MBX-APIKEY") != ""
		switch r.Method {
		case http.MethodPost:
			f.posts++
			f.sawClientOrderID = q.Get("newClientOrderId")
			f.sawType = q.Get("type")
			w.WriteHeader(f.newOrderStatus)
			_, _ = w.Write([]byte(f.newOrderBody))
		case http.MethodDelete:
			f.deletes++
			f.sawDeleteCl = q.Get("origClientOrderId")
			_, _ = w.Write([]byte(f.cancelBody))
		default:
			_, _ = w.Write([]byte(f.queryBody)) // GET: query-order
		}
	})
	mux.HandleFunc("/api/v3/account", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(f.accountBody))
	})
	mux.HandleFunc("/api/v3/ticker/price", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"symbol":"` + r.URL.Query().Get("symbol") + `","price":"` + f.tickerPrice + `"}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func fullFill(clientOrderID string) string {
	return `{"symbol":"BTCUSDT","orderId":1,"clientOrderId":"` + clientOrderID + `","status":"FILLED",` +
		`"executedQty":"1.00000000","fills":[{"price":"50000.00","qty":"1.00000000","commission":"0.001","commissionAsset":"BNB","tradeId":42}]}`
}

func venueOverFake(f *fakeBinance) *BinanceVenue {
	bucket := newWeightBucket(1200, time.Minute, nil)
	rest := newBinanceREST(restConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Bucket: bucket})
	return NewBinanceVenue(BinanceConfig{MIC: "BINANCE", Symbols: StaticSymbolMap{"BTC-USD": "BTCUSDT"}, REST: rest})
}

func bdec(v string) *commonpb.Decimal {
	r, _ := new(big.Rat).SetString(v)
	return dec.ToProto(r)
}

func marketOrder(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: id, InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_MARKET, OrderedQuantity: bdec("1"),
	}
}

func TestBinance_MarketOrderFillsWithClientOrderID(t *testing.T) {
	f := newFakeBinance(t)
	f.newOrderBody = fullFill("ORDER-123")
	v := venueOverFake(f)

	fills, err := v.Execute(context.Background(), marketOrder("ORDER-123"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// newClientOrderId handshake: our deterministic order_id went to Binance.
	if f.sawClientOrderID != "ORDER-123" {
		t.Fatalf("newClientOrderId = %q, want ORDER-123", f.sawClientOrderID)
	}
	if !f.sawSignature || !f.sawAPIKey {
		t.Fatalf("request not signed/authenticated: sig=%v key=%v", f.sawSignature, f.sawAPIKey)
	}
	if f.sawType != "MARKET" {
		t.Fatalf("type = %q, want MARKET", f.sawType)
	}
	if len(fills) != 1 {
		t.Fatalf("fills = %d, want 1", len(fills))
	}
	if got := dec.FromProto(fills[0].GetQuantity()); got.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("fill qty = %s, want 1", got.RatString())
	}
	if got := dec.FromProto(fills[0].GetPrice()); got.Cmp(big.NewRat(50000, 1)) != 0 {
		t.Fatalf("fill price = %s, want 50000", got.RatString())
	}
	if fills[0].GetVenue() != "BINANCE" || fills[0].GetVenueExecutionId() != "42" {
		t.Fatalf("venue/exec id wrong: %+v", fills[0])
	}
}

func TestBinance_RestingLimitReturnsNoFills(t *testing.T) {
	f := newFakeBinance(t)
	f.newOrderBody = `{"symbol":"BTCUSDT","orderId":2,"clientOrderId":"L1","status":"NEW","executedQty":"0","fills":[]}`
	v := venueOverFake(f)
	st := &orderpb.OrderState{
		OrderId: "L1", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: bdec("1"), LimitPrice: bdec("40000"),
	}
	fills, err := v.Execute(context.Background(), st)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fills) != 0 {
		t.Fatalf("resting limit fills = %d, want 0", len(fills))
	}
	if f.sawType != "LIMIT" {
		t.Fatalf("type = %q, want LIMIT", f.sawType)
	}
}

func TestBinance_DuplicateRecoversViaQuery(t *testing.T) {
	f := newFakeBinance(t)
	// New-order returns the -2010 duplicate error; the venue recovers the true
	// state via queryOrder (idempotency), returning the ORIGINAL fills.
	f.newOrderStatus = 400
	f.newOrderBody = `{"code":-2010,"msg":"Duplicate order sent."}`
	f.queryBody = fullFill("DUP-1")
	v := venueOverFake(f)

	fills, err := v.Execute(context.Background(), marketOrder("DUP-1"))
	if err != nil {
		t.Fatalf("Execute (recover): %v", err)
	}
	if len(fills) != 1 {
		t.Fatalf("recovered fills = %d, want 1 (no double-execution)", len(fills))
	}
}

func TestBinance_UnrecoverableRejectionSurfaces(t *testing.T) {
	f := newFakeBinance(t)
	f.newOrderStatus = 400
	f.newOrderBody = `{"code":-2010,"msg":"Account has insufficient balance."}`
	f.queryBody = `{"code":-2013,"msg":"Order does not exist."}` // query confirms it never landed
	v := venueOverFake(f)

	if _, err := v.Execute(context.Background(), marketOrder("REJ-1")); err == nil {
		t.Fatal("expected an error — a rejected order must never fabricate a fill")
	}
}

func TestBinance_RateLimitBudgetExhausted(t *testing.T) {
	f := newFakeBinance(t)
	f.newOrderBody = fullFill("RL-1")
	// A zero-capacity bucket: the very first call is denied.
	bucket := newWeightBucket(0, time.Minute, func() time.Time { return time.Unix(0, 0) })
	throttled := false
	rest := newBinanceREST(restConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Bucket: bucket, OnThrottle: func() { throttled = true }})
	v := NewBinanceVenue(BinanceConfig{MIC: "BINANCE", Symbols: StaticSymbolMap{"BTC-USD": "BTCUSDT"}, REST: rest})

	_, err := v.Execute(context.Background(), marketOrder("RL-1"))
	if err == nil {
		t.Fatal("expected ErrRateLimited, got nil")
	}
	if !throttled {
		t.Fatal("structural-alert hook (OnThrottle) not fired on budget exhaustion")
	}
	if f.sawClientOrderID != "" {
		t.Fatal("an order was placed despite the budget being exhausted — must back off, never fire")
	}
}

func TestBinance_UnmappedSymbolRejected(t *testing.T) {
	f := newFakeBinance(t)
	v := venueOverFake(f)
	st := marketOrder("X")
	st.InstrumentId = "DOGE-USD" // not in the symbol map
	if _, err := v.Execute(context.Background(), st); err == nil {
		t.Fatal("expected an error for an unmapped instrument")
	}
}
