package okx

import (
	"context"
	"encoding/json"
	"github.com/eighred/kanz/internal/venueadapter/exchangeauth"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

type fakeOKX struct {
	srv         *httptest.Server
	sawSign     bool
	sawKey      bool
	sawPass     bool
	sawClOrd    string
	sawPostCl   string
	sawPostSz   string // the size actually sent to the venue (#94)
	posts       int
	placeBody   string
	queryBody   string
	balanceBody string
	tickerBody  string
	configBody  string // body for GET /api/v5/account/config — carries the account uid
}

func newFakeOKX(t *testing.T) *fakeOKX {
	t.Helper()
	f := &fakeOKX{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v5/trade/order", func(w http.ResponseWriter, r *http.Request) {
		f.sawSign = r.Header.Get("OK-ACCESS-SIGN") != ""
		f.sawKey = r.Header.Get("OK-ACCESS-KEY") != ""
		f.sawPass = r.Header.Get("OK-ACCESS-PASSPHRASE") != ""
		if r.Method == http.MethodPost {
			f.posts++
			var body struct {
				ClOrdID string `json:"clOrdId"`
				Sz      string `json:"sz"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.sawPostCl = body.ClOrdID
			f.sawPostSz = body.Sz
			_, _ = w.Write([]byte(f.placeBody))
			return
		}
		f.sawClOrd = r.URL.Query().Get("clOrdId")
		_, _ = w.Write([]byte(f.queryBody))
	})
	mux.HandleFunc("/api/v5/account/balance", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(f.balanceBody))
	})
	mux.HandleFunc("/api/v5/market/ticker", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(f.tickerBody))
	})
	mux.HandleFunc("/api/v5/account/config", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(f.configBody))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func odec(v string) *commonpb.Decimal {
	r, _ := new(big.Rat).SetString(v)
	return dec.ToProto(r)
}

func okxVenueOver(f *fakeOKX) *OKXVenue {
	bucket := NewWeightBucket(60, 2*time.Second, nil)
	rest := newOKXREST(okxRestConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Passphrase: "p", Bucket: bucket, Mode: exchangeauth.OKXDemo})
	return &OKXVenue{mic: "OKX", rest: rest, symbols: StaticSymbolMap{"BTC-USD": "BTC-USDT"}, now: time.Now}
}

func okxMarket(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: id, InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_MARKET, OrderedQuantity: odec("1"),
	}
}

func TestOKX_MarketOrderFills(t *testing.T) {
	f := newFakeOKX(t)
	f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","sCode":"0","sMsg":""}]}`
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled","accFillSz":"1","avgPx":"50000","fee":"-0.05","feeCcy":"USDT"}]}`
	v := okxVenueOver(f)

	fills, err := v.Execute(context.Background(), okxMarket("o1"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !f.sawSign || !f.sawKey || !f.sawPass {
		t.Fatalf("request not fully signed: sign=%v key=%v pass=%v", f.sawSign, f.sawKey, f.sawPass)
	}
	if len(fills) != 1 {
		t.Fatalf("fills = %d, want 1", len(fills))
	}
	if got := dec.FromProto(fills[0].GetQuantity()); got.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("qty = %s, want 1", got.RatString())
	}
	if got := dec.FromProto(fills[0].GetPrice()); got.Cmp(big.NewRat(50000, 1)) != 0 {
		t.Fatalf("price = %s, want 50000", got.RatString())
	}
	if fills[0].GetVenue() != "OKX" || fills[0].GetVenueExecutionId() != "312" {
		t.Fatalf("venue/exec id wrong: %+v", fills[0])
	}
	// Fee magnitude stored (OKX reports it negative).
	if got := dec.FromProto(fills[0].GetFee().GetAmount()); got.Cmp(big.NewRat(1, 20)) != 0 { // 0.05
		t.Fatalf("fee = %s, want 0.05", got.RatString())
	}
}

func TestOKX_DuplicateRecoversViaQuery(t *testing.T) {
	f := newFakeOKX(t)
	// Place fails with a duplicate-clOrdId sCode; query recovers the true state.
	f.placeBody = `{"code":"1","msg":"","data":[{"ordId":"","clOrdId":"o1","sCode":"51000","sMsg":"Duplicate clOrdId"}]}`
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled","accFillSz":"1","avgPx":"50000"}]}`
	v := okxVenueOver(f)

	fills, err := v.Execute(context.Background(), okxMarket("o1"))
	if err != nil {
		t.Fatalf("Execute (recover): %v", err)
	}
	if len(fills) != 1 {
		t.Fatalf("recovered fills = %d, want 1 (no double-execution)", len(fills))
	}
	if f.sawClOrd != "o1" {
		t.Fatalf("recovery queried clOrdId %q, want o1", f.sawClOrd)
	}
}

func TestOKX_RestingLimitNoFills(t *testing.T) {
	f := newFakeOKX(t)
	f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"9","clOrdId":"L1","sCode":"0","sMsg":""}]}`
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"9","clOrdId":"L1","state":"live","accFillSz":"0","avgPx":""}]}`
	v := okxVenueOver(f)
	st := &orderpb.OrderState{
		OrderId: "L1", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: odec("1"), LimitPrice: odec("40000"),
	}
	fills, err := v.Execute(context.Background(), st)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fills) != 0 {
		t.Fatalf("resting limit fills = %d, want 0", len(fills))
	}
}

func TestOKX_UnmappedSymbolRejected(t *testing.T) {
	f := newFakeOKX(t)
	v := okxVenueOver(f)
	st := okxMarket("x")
	st.InstrumentId = "DOGE-USD"
	if _, err := v.Execute(context.Background(), st); err == nil {
		t.Fatal("expected an error for an unmapped instrument")
	}
}
