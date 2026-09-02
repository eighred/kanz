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
	gets        int // GET /api/v5/trade/order count — a reconciler query-order (#904)
	placeBody   string
	queryBody   string
	balanceBody string
	tickerBody  string
	configBody  string // body for GET /api/v5/account/config — carries the account uid
	// GET /api/v5/trade/order-algo — where a CONDITIONAL order lives (#485). It
	// is a separate endpoint because /trade/order answers 51603 for a stop that
	// is resting and perfectly healthy, which is the one place that code must
	// NOT be read as "the venue never had it".
	algoQueryBody string
	algoGets      int
	sawAlgoClOrd  string
	// GET /api/v5/trade/fills-history — the individual EXECUTIONS behind an
	// order (#923). This is where a fill's identity comes from: tradeId, the
	// same field the private user-data stream carries, rather than ordId.
	//
	// The default is an EMPTY trade list rather than a nil body, so a test that
	// does not set it exercises "OKX reports the order traded and returns no
	// trade for it" — which must never become a fabricated fill.
	fillsBody      string
	fillsGets      int
	sawFillsOrdID  string
	sawFillsInstID string
}

func newFakeOKX(t *testing.T) *fakeOKX {
	t.Helper()
	f := &fakeOKX{fillsBody: `{"code":"0","msg":"","data":[]}`}
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
		f.gets++
		f.sawClOrd = r.URL.Query().Get("clOrdId")
		_, _ = w.Write([]byte(f.queryBody))
	})
	mux.HandleFunc("/api/v5/trade/order-algo", func(w http.ResponseWriter, r *http.Request) {
		f.algoGets++
		f.sawAlgoClOrd = r.URL.Query().Get("algoClOrdId")
		_, _ = w.Write([]byte(f.algoQueryBody))
	})
	mux.HandleFunc("/api/v5/trade/fills-history", func(w http.ResponseWriter, r *http.Request) {
		f.fillsGets++
		f.sawFillsOrdID = r.URL.Query().Get("ordId")
		f.sawFillsInstID = r.URL.Query().Get("instId")
		_, _ = w.Write([]byte(f.fillsBody))
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
	rest := newOKXREST(okxRestConfig{BaseURL: f.srv.URL, APIKey: "k", APISecret: "s", Passphrase: "p", Buckets: newOKXBuckets(bucket, nil), Mode: exchangeauth.OKXDemo})
	return &OKXVenue{mic: "OKX", rest: rest, symbols: StaticSymbolMap{"BTC-USD": "BTC-USDT"}, now: time.Now}
}

func okxMarket(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: id, InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_MARKET, OrderedQuantity: odec("1"),
	}
}

// THE PLACEMENT PATH REPORTS ONE FILL PER TRADE, NAMED "<instId>-<tradeId>" (#923).
//
// It used to synthesize ONE CUMULATIVE fill per order from accFillSz and avgPx,
// named "<instId>-<ordId>". That is a different OKX identifier space from the
// one the private user-data stream uses, so one execution reached the order
// aggregate and the position book under two names — and both dedup on that name.
func TestOKX_MarketOrderFills(t *testing.T) {
	f := newFakeOKX(t)
	f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","sCode":"0","sMsg":""}]}`
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled","accFillSz":"1","avgPx":"50000","fee":"-0.05","feeCcy":"USDT"}]}`
	f.fillsBody = `{"code":"0","msg":"","data":[` +
		`{"instId":"BTC-USDT","tradeId":"7","ordId":"312","clOrdId":"o1","fillPx":"50000","fillSz":"0.6","fee":"-0.03","feeCcy":"USDT","ts":"1700000000000"},` +
		`{"instId":"BTC-USDT","tradeId":"8","ordId":"312","clOrdId":"o1","fillPx":"50000","fillSz":"0.4","fee":"-0.02","feeCcy":"USDT","ts":"1700000001000"}]}`
	v := okxVenueOver(f)

	fills, err := v.Execute(context.Background(), okxMarket("o1"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !f.sawSign || !f.sawKey || !f.sawPass {
		t.Fatalf("request not fully signed: sign=%v key=%v pass=%v", f.sawSign, f.sawKey, f.sawPass)
	}
	if len(fills) != 2 {
		t.Fatalf("fills = %d, want 2 — one per TRADE, not one aggregate over the order", len(fills))
	}
	// THE IDENTITIES, and they are the whole point. okx_userdata.go mints exactly
	// these for the same two trades off the private websocket.
	if fills[0].GetFillId() != "BTC-USDT-7" || fills[1].GetFillId() != "BTC-USDT-8" {
		t.Fatalf("fill ids = %q, %q; want BTC-USDT-7 and BTC-USDT-8 — the ids the user-data "+
			"stream mints for these trades, or the same execution folds twice under two names",
			fills[0].GetFillId(), fills[1].GetFillId())
	}
	if got := dec.FromProto(fills[0].GetQuantity()); got.Cmp(big.NewRat(6, 10)) != 0 {
		t.Fatalf("fill 1 qty = %s, want 0.6", got.RatString())
	}
	if got := dec.FromProto(fills[1].GetPrice()); got.Cmp(big.NewRat(50000, 1)) != 0 {
		t.Fatalf("fill 2 price = %s, want 50000", got.RatString())
	}
	if fills[0].GetVenue() != "OKX" || fills[0].GetVenueExecutionId() != "7" {
		t.Fatalf("venue/exec id wrong: %+v", fills[0])
	}
	// Fee magnitude stored per trade (OKX reports it negative).
	if got := dec.FromProto(fills[0].GetFee().GetAmount()); got.Cmp(big.NewRat(3, 100)) != 0 { // 0.03
		t.Fatalf("fee = %s, want 0.03", got.RatString())
	}
	// THE TRADE'S OWN INSTANT, not the connector's clock.
	if got := fills[0].GetExecutedAt().AsTime(); !got.Equal(time.UnixMilli(1700000000000).UTC()) {
		t.Fatalf("executed_at = %s, want the exchange's own trade time", got)
	}
	// ADDRESSED BY OKX'S OWN ordId — fills-history has no clOrdId form.
	if f.sawFillsOrdID != "312" || f.sawFillsInstID != "BTC-USDT" {
		t.Fatalf("fills-history asked about ordId %q instId %q, want 312 / BTC-USDT",
			f.sawFillsOrdID, f.sawFillsInstID)
	}
}

func TestOKX_DuplicateRecoversViaQuery(t *testing.T) {
	f := newFakeOKX(t)
	// Place fails with a duplicate-clOrdId sCode; query recovers the true state.
	f.placeBody = `{"code":"1","msg":"","data":[{"ordId":"","clOrdId":"o1","sCode":"51000","sMsg":"Duplicate clOrdId"}]}`
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled","accFillSz":"1","avgPx":"50000"}]}`
	f.fillsBody = `{"code":"0","msg":"","data":[{"instId":"BTC-USDT","tradeId":"7","ordId":"312",` +
		`"clOrdId":"o1","fillPx":"50000","fillSz":"1","ts":"1700000000000"}]}`
	v := okxVenueOver(f)

	fills, err := v.Execute(context.Background(), okxMarket("o1"))
	if err != nil {
		t.Fatalf("Execute (recover): %v", err)
	}
	if len(fills) != 1 {
		t.Fatalf("recovered fills = %d, want 1 (no double-execution)", len(fills))
	}
	if fills[0].GetFillId() != "BTC-USDT-7" {
		t.Fatalf("recovered fill_id = %q, want BTC-USDT-7 — the recovery path and the user-data "+
			"stream must name one execution once", fills[0].GetFillId())
	}
	if f.sawClOrd != "o1" {
		t.Fatalf("recovery queried clOrdId %q, want o1", f.sawClOrd)
	}
}

// A RESTING ORDER COSTS ONE REQUEST, NOT SEVEN. Nothing traded, so there is
// nothing to ask fills-history about — and that endpoint's own rate limit is six
// times tighter than the placement endpoint's, so spending it on every resting
// limit would starve the path that places orders.
func TestOKX_NoTradesFetchedForAnOrderThatDidNotTrade(t *testing.T) {
	f := newFakeOKX(t)
	f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"9","clOrdId":"L1","sCode":"0","sMsg":""}]}`
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"9","clOrdId":"L1","state":"live","accFillSz":"0","avgPx":""}]}`
	v := okxVenueOver(f)

	st := &orderpb.OrderState{
		OrderId: "L1", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: odec("1"), LimitPrice: odec("40000"),
	}
	if _, err := v.Execute(context.Background(), st); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if f.fillsGets != 0 {
		t.Fatalf("%d fills-history requests fired for an order that traded nothing", f.fillsGets)
	}
}

// OKX SAYING "FILLED" AND RETURNING NO TRADE PRODUCES NO FILL — NOT A
// FABRICATED ONE.
//
// This is the propagation window: the order query already shows accFillSz while
// the trade has not reached the executions endpoint. The synchronous path
// reports nothing and the user-data stream and the reconciler heal it, which is
// the same net that already covers an order that is not yet queryable at all.
// Synthesizing a cumulative fill to cover the gap is exactly what #923 removed.
func TestOKX_ATradedOrderWithNoTradeRecordFabricatesNothing(t *testing.T) {
	f := newFakeOKX(t)
	f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","sCode":"0","sMsg":""}]}`
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled","accFillSz":"1","avgPx":"50000"}]}`
	// fillsBody keeps its default: code 0, empty data.

	fills, err := okxVenueOver(f).Execute(context.Background(), okxMarket("o1"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fills) != 0 {
		t.Fatalf("fills = %+v; OKX named no trade, so this adapter must name none either — a fill "+
			"invented here is a second name for an execution the websocket will deliver", fills)
	}
	if f.fillsGets != 1 {
		t.Fatalf("fills-history requests = %d, want 1", f.fillsGets)
	}
}

// A TRADE ROW WITHOUT AN instId IS STILL NAMED THE WAY THE WEBSOCKET NAMES IT.
//
// The user-data push always carries instId, so a REST row that omitted it would
// split the id space again — "-7" here against "BTC-USDT-7" there, for one
// trade. The adapter's own mapped venue symbol is the same string OKX puts on
// the push, so falling back to it keeps the two paths agreeing.
func TestOKX_ATradeRowWithoutAnInstIdStillCarriesTheVenueSymbol(t *testing.T) {
	f := newFakeOKX(t)
	f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","sCode":"0","sMsg":""}]}`
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled","accFillSz":"1","avgPx":"50000"}]}`
	f.fillsBody = `{"code":"0","msg":"","data":[{"tradeId":"7","ordId":"312","clOrdId":"o1",` +
		`"fillPx":"50000","fillSz":"1","ts":"1700000000000"}]}`

	fills, err := okxVenueOver(f).Execute(context.Background(), okxMarket("o1"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fills) != 1 || fills[0].GetFillId() != "BTC-USDT-7" {
		t.Fatalf("fills = %+v; want one fill named BTC-USDT-7 — an id missing the instrument half "+
			"is a different name for the trade the user-data stream will deliver", fills)
	}
}

// A TRADED ORDER WITH NO ordId IS A REFUSAL, NOT AN EMPTY FILL SET.
//
// fills-history is addressed by OKX's own ordId and has no clOrdId form, so an
// order OKX says traded while naming no order id is one this adapter has no
// second way to ask about. Returning no fills would report a real execution as
// no execution — and it is exactly what the old code did, silently, by falling
// back to OUR order id and building a synthetic "<instId>-<orderId>" fill.
func TestOKX_ATradedOrderWithNoOrdIdIsRefusedNotSilentlyEmpty(t *testing.T) {
	f := newFakeOKX(t)
	f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","sCode":"0","sMsg":""}]}`
	f.queryBody = `{"code":"0","msg":"","data":[{"clOrdId":"o1","state":"filled","accFillSz":"1","avgPx":"50000"}]}`

	fills, err := okxVenueOver(f).Execute(context.Background(), okxMarket("o1"))
	if err == nil {
		t.Fatalf("OKX reported a trade and named no ordId, and Execute returned no error "+
			"(fills=%+v). There is no way to ask which trades made it up, so silence here books "+
			"nothing for an execution that happened", fills)
	}
	if f.fillsGets != 0 {
		t.Fatalf("%d fills-history requests fired without an ordId to address", f.fillsGets)
	}
}

// A ZERO-SIZE ROW IS NOT A TRADE; AN UNREADABLE ONE IS AN ERROR.
//
// The two must not collapse. A zero-size row skipped as a fill keeps a
// zero-quantity execution out of the position book; a GARBLED size skipped the
// same way would report a real execution as no execution, which is the failure
// #94's error return exists for.
func TestOKX_AZeroSizeRowIsSkippedAndAGarbledOneIsRefused(t *testing.T) {
	t.Run("zero size is not a trade", func(t *testing.T) {
		f := newFakeOKX(t)
		f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","sCode":"0","sMsg":""}]}`
		f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled","accFillSz":"1","avgPx":"50000"}]}`
		f.fillsBody = `{"code":"0","msg":"","data":[` +
			`{"instId":"BTC-USDT","tradeId":"6","ordId":"312","fillPx":"0","fillSz":"0","fee":"-0.01","feeCcy":"USDT","ts":"1700000000000"},` +
			`{"instId":"BTC-USDT","tradeId":"7","ordId":"312","fillPx":"50000","fillSz":"1","ts":"1700000001000"}]}`

		fills, err := okxVenueOver(f).Execute(context.Background(), okxMarket("o1"))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if len(fills) != 1 || fills[0].GetFillId() != "BTC-USDT-7" {
			t.Fatalf("fills = %+v; want only BTC-USDT-7 — a zero-size row is a fee record, not an "+
				"execution, and folding it puts a zero-quantity fill in the position book", fills)
		}
	})

	t.Run("an unreadable size is refused, never skipped", func(t *testing.T) {
		f := newFakeOKX(t)
		f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","sCode":"0","sMsg":""}]}`
		f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled","accFillSz":"1","avgPx":"50000"}]}`
		f.fillsBody = `{"code":"0","msg":"","data":[{"instId":"BTC-USDT","tradeId":"7","ordId":"312",` +
			`"fillPx":"50000","fillSz":"n/a","ts":"1700000000000"}]}`

		fills, err := okxVenueOver(f).Execute(context.Background(), okxMarket("o1"))
		if err == nil {
			t.Fatalf("a trade whose size could not be read produced no error (fills=%+v). Skipping "+
				"it reports a real execution as no execution, and nothing downstream can notice", fills)
		}
	})
}

// A REFUSED TRADE FETCH IS AN ERROR, NOT AN ORDER THAT FILLED NOTHING.
//
// The order query succeeded and said the order traded; the answer is still
// incomplete. Reporting no fills here would report a real execution as no
// execution, which is what #94's error return exists to prevent.
func TestOKX_ARefusedTradeFetchIsAnError(t *testing.T) {
	f := newFakeOKX(t)
	f.placeBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","sCode":"0","sMsg":""}]}`
	f.queryBody = `{"code":"0","msg":"","data":[{"ordId":"312","clOrdId":"o1","state":"filled","accFillSz":"1","avgPx":"50000"}]}`
	f.fillsBody = `{"code":"50011","msg":"Requests too frequent","data":[]}`

	fills, err := okxVenueOver(f).Execute(context.Background(), okxMarket("o1"))
	if err == nil {
		t.Fatal("a refused fills-history produced no error — a traded order would be recorded as " +
			"having traded nothing, and nothing downstream could notice")
	}
	if len(fills) != 0 {
		t.Fatalf("fills = %+v alongside an error", fills)
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
