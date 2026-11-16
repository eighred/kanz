package binance

import (
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

func preciseOrder(kind orderpb.OrderType) *orderpb.OrderState {
	st := marketOrder("precision1594")
	st.OrderType = kind
	st.OrderedQuantity = &commonpb.Decimal{Coefficient: 1, Exponent: -9}
	st.LimitPrice = &commonpb.Decimal{Coefficient: 1000000009, Exponent: -9}
	st.StopPrice = &commonpb.Decimal{Coefficient: 1000000001, Exponent: -9}
	return st
}

func TestBinanceExactDecimalWire(t *testing.T) {
	for _, kind := range []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_MARKET,
		orderpb.OrderType_ORDER_TYPE_LIMIT, orderpb.OrderType_ORDER_TYPE_STOP,
		orderpb.OrderType_ORDER_TYPE_STOP_LIMIT} {
		t.Run(kind.String(), func(t *testing.T) {
			requests := make(chan url.Values, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.URL.Query()
				_, _ = w.Write([]byte(`{"status":"NEW","executedQty":"0","fills":[]}`))
			}))
			defer srv.Close()
			if _, err := venueOverFake(&fakeBinance{srv: srv}).Execute(context.Background(), preciseOrder(kind)); err != nil {
				t.Fatal(err)
			}
			var q url.Values
			select {
			case q = <-requests:
			default:
				t.Fatal("no placement request observed")
			}
			if q.Get("quantity") != "0.000000001" || q.Get("signature") == "" {
				t.Fatal("quantity lost precision or request was unsigned")
			}
			if kind == orderpb.OrderType_ORDER_TYPE_LIMIT || kind == orderpb.OrderType_ORDER_TYPE_STOP_LIMIT {
				if q.Get("price") != "1.000000009" {
					t.Fatalf("price = %q", q.Get("price"))
				}
			}
			if kind == orderpb.OrderType_ORDER_TYPE_STOP || kind == orderpb.OrderType_ORDER_TYPE_STOP_LIMIT {
				if q.Get("stopPrice") != "1.000000001" {
					t.Fatalf("stopPrice = %q", q.Get("stopPrice"))
				}
			}
		})
	}
}

func TestBinanceUnsupportedDecimalRefusesBeforeHTTP(t *testing.T) {
	requests := make(chan struct{}, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests <- struct{}{}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	v := venueOverFake(&fakeBinance{srv: srv})
	for _, field := range []string{"quantity", "price", "stopPrice"} {
		for _, value := range []*commonpb.Decimal{nil, {Coefficient: 1, Exponent: 2147483647}, {Coefficient: 1, Exponent: -61}} {
			st := preciseOrder(orderpb.OrderType_ORDER_TYPE_STOP_LIMIT)
			switch field {
			case "quantity":
				st.OrderedQuantity = value
			case "price":
				st.LimitPrice = value
			case "stopPrice":
				st.StopPrice = value
			}
			if fills, err := v.Execute(context.Background(), st); err == nil || len(fills) != 0 {
				t.Fatalf("%s: unsupported value was accepted", field)
			}
		}
	}
	select {
	case <-requests:
		t.Fatal("invalid decimal reached HTTP")
	default:
	}
}

func TestBinanceSubEightPlaceFillAndCommission(t *testing.T) {
	response := &orderResponse{Symbol: "BTCUSDT", Fills: []orderFill{{TradeID: 7,
		Qty: "0.000000001", Price: "1.000000009", Commission: "0.000000003", CommissionAsset: "USDT"}}}
	v := venueOverFake(newFakeBinance(t))
	fills, err := v.fills(response, preciseOrder(orderpb.OrderType_ORDER_TYPE_MARKET))
	if err != nil || len(fills) != 1 {
		t.Fatalf("fills: %v, %v", fills, err)
	}
	assertPreciseFill(t, fills[0])
	cap := &reconCapture{}
	frame := `{"e":"executionReport","s":"BTCUSDT","c":"o1","x":"TRADE","X":"PARTIALLY_FILLED","l":"0.000000001","L":"1.000000009","z":"0.000000001","t":7,"n":"0.000000003","N":"USDT","T":1700000000000}`
	if err := ingesterOver([][]byte{[]byte(frame)}, cap).Run(context.Background()); len(cap.events) != 1 {
		t.Fatalf("stream emitted %d events: %v", len(cap.events), err)
	}
	event := cap.events[0].Payload.(*orderpb.OrderPartiallyFilled)
	assertPreciseFill(t, event.GetFill())
	if got := dec.FromProto(event.GetState().GetLeavesQuantity()); got.Cmp(big.NewRat(999999999, 1000000000)) != 0 {
		t.Fatalf("stream leaves = %s", got.RatString())
	}
}

func assertPreciseFill(t *testing.T, fill *orderpb.Fill) {
	t.Helper()
	for _, pair := range []struct {
		got  *commonpb.Decimal
		want *big.Rat
	}{
		{fill.GetQuantity(), big.NewRat(1, 1000000000)},
		{fill.GetPrice(), big.NewRat(1000000009, 1000000000)},
		{fill.GetFee().GetAmount(), big.NewRat(3, 1000000000)},
	} {
		if pair.got == nil || dec.FromProto(pair.got).Cmp(pair.want) != 0 {
			t.Fatalf("got %v, want %s", pair.got, pair.want.RatString())
		}
	}
}

func TestBinanceUnsupportedFillRefusesWholeReport(t *testing.T) {
	base := `{"e":"executionReport","s":"BTCUSDT","c":"o1","x":"TRADE","X":"PARTIALLY_FILLED","l":"0.1","L":"1","z":"0.1","t":7,"n":"0.01","N":"USDT","T":1700000000000}`
	for _, field := range []string{`"l":"0.1"`, `"L":"1"`, `"z":"0.1"`, `"n":"0.01"`} {
		for _, value := range []string{"1e999999999", "1.1234567890123456789"} {
			cap := &reconCapture{}
			view := newFakeOrders("o1")
			frame := strings.Replace(base, field, strings.Split(field, ":")[0]+`:"`+value+`"`, 1)
			err := ingesterOverView(nil, cap, view).handle(context.Background(), []byte(frame))
			if err == nil && view.get(t, "o1").GetQuarantine() == nil {
				t.Fatalf("unsupported field %s neither refused nor quarantined", field)
			}
			if len(cap.events) != 0 {
				t.Fatal("refused report published a financial fact")
			}
		}
	}
}
