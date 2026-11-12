package okx

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

func preciseOrder(kind orderpb.OrderType) *orderpb.OrderState {
	st := okxMarket("precision1594")
	st.OrderType = kind
	st.OrderedQuantity = &commonpb.Decimal{Coefficient: 1, Exponent: -9}
	st.LimitPrice = &commonpb.Decimal{Coefficient: 1000000009, Exponent: -9}
	st.StopPrice = &commonpb.Decimal{Coefficient: 1000000001, Exponent: -9}
	return st
}

func TestOKXExactDecimalWire(t *testing.T) {
	for _, kind := range []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_MARKET,
		orderpb.OrderType_ORDER_TYPE_LIMIT, orderpb.OrderType_ORDER_TYPE_STOP,
		orderpb.OrderType_ORDER_TYPE_STOP_LIMIT} {
		t.Run(kind.String(), func(t *testing.T) {
			requests := make(chan map[string]string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					var body map[string]string
					if json.NewDecoder(r.Body).Decode(&body) != nil || r.Header.Get("OK-ACCESS-SIGN") == "" {
						body = nil
					}
					requests <- body
					_, _ = w.Write([]byte(`{"code":"0","data":[{"ordId":"312","algoId":"312","sCode":"0"}]}`))
					return
				}
				_, _ = w.Write([]byte(`{"code":"0","data":[{"ordId":"312","state":"live","accFillSz":"0"}]}`))
			}))
			defer srv.Close()
			if _, err := okxVenueOver(&fakeOKX{srv: srv}).Execute(context.Background(), preciseOrder(kind)); err != nil {
				t.Fatal(err)
			}
			var body map[string]string
			select {
			case body = <-requests:
			default:
				t.Fatal("no placement request observed")
			}
			if body["sz"] != "0.000000001" {
				t.Fatal("quantity lost precision or request was unsigned")
			}
			if kind == orderpb.OrderType_ORDER_TYPE_LIMIT && body["px"] != "1.000000009" {
				t.Fatalf("px = %q", body["px"])
			}
			if kind == orderpb.OrderType_ORDER_TYPE_STOP || kind == orderpb.OrderType_ORDER_TYPE_STOP_LIMIT {
				if body["triggerPx"] != "1.000000001" {
					t.Fatalf("triggerPx = %q", body["triggerPx"])
				}
				want := "-1" // OKX's market-on-trigger sentinel must survive.
				if kind == orderpb.OrderType_ORDER_TYPE_STOP_LIMIT {
					want = "1.000000009"
				}
				if body["orderPx"] != want {
					t.Fatalf("orderPx = %q, want %s", body["orderPx"], want)
				}
			}
		})
	}
}

func TestOKXUnsupportedDecimalRefusesBeforeHTTP(t *testing.T) {
	requests := make(chan struct{}, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests <- struct{}{}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	v := okxVenueOver(&fakeOKX{srv: srv})
	for _, kind := range []orderpb.OrderType{orderpb.OrderType_ORDER_TYPE_LIMIT, orderpb.OrderType_ORDER_TYPE_STOP_LIMIT} {
		for _, field := range []string{"quantity", "price", "stopPrice"} {
			if kind == orderpb.OrderType_ORDER_TYPE_LIMIT && field == "stopPrice" {
				continue
			}
			for _, value := range []*commonpb.Decimal{nil, {Coefficient: 1, Exponent: 2147483647}, {Coefficient: 1, Exponent: -61}} {
				st := preciseOrder(kind)
				switch field {
				case "quantity":
					st.OrderedQuantity = value
				case "price":
					st.LimitPrice = value
				case "stopPrice":
					st.StopPrice = value
				}
				if fills, err := v.Execute(context.Background(), st); err == nil || len(fills) != 0 {
					t.Fatalf("%s/%s: unsupported value accepted", kind, field)
				}
			}
		}
	}
	select {
	case <-requests:
		t.Fatal("invalid decimal reached HTTP")
	default:
	}
}

func TestOKXSubEightPlaceFillAndCommission(t *testing.T) {
	v := okxVenueOver(newFakeOKX(t))
	fills, err := v.fills([]okxFill{{TradeID: "7", FillSz: "0.000000001", FillPx: "1.000000009",
		Fee: "-0.000000003", FeeCcy: "USDT", TS: "1700000000000"}}, preciseOrder(orderpb.OrderType_ORDER_TYPE_MARKET), "BTC-USDT")
	if err != nil || len(fills) != 1 {
		t.Fatalf("fills: %v, %v", fills, err)
	}
	assertPreciseFill(t, fills[0])
	cap := &okxCapture{}
	frame := `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","clOrdId":"o1","state":"partially_filled","fillSz":"0.000000001","fillPx":"1.000000009","accFillSz":"0.000000001","tradeId":"7","fillFee":"-0.000000003","fillFeeCcy":"USDT","uTime":"1700000000000"}]}`
	if err := okxIngesterOver([][]byte{[]byte(frame)}, cap).Run(context.Background()); len(cap.events) != 1 {
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

func TestOKXUnsupportedFillRefusesWholeReport(t *testing.T) {
	base := `{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","clOrdId":"o1","state":"partially_filled","fillSz":"0.1","fillPx":"1","accFillSz":"0.1","tradeId":"7","fillFee":"-0.01","fillFeeCcy":"USDT","uTime":"1700000000000"}]}`
	for _, field := range []string{`"fillSz":"0.1"`, `"fillPx":"1"`, `"accFillSz":"0.1"`, `"fillFee":"-0.01"`} {
		for _, value := range []string{"1e999999999", "1.1234567890123456789"} {
			cap := &okxCapture{}
			frame := strings.Replace(base, field, strings.Split(field, ":")[0]+`:"`+value+`"`, 1)
			if err := okxIngesterOver(nil, cap).handle(context.Background(), []byte(frame)); err == nil {
				t.Fatalf("unsupported field %s was accepted", field)
			}
			if len(cap.events) != 0 {
				t.Fatal("refused report published a financial fact")
			}
		}
	}
}

func TestOKXFeeSignConversionIsExactOrRefuses(t *testing.T) {
	for _, text := range []string{"-0.000000003", "0.000000003"} {
		money, ok := okxFee(text, "USDT")
		if !ok || money == nil || dec.FromProto(money.Amount).Cmp(big.NewRat(3, 1000000000)) != 0 {
			t.Fatalf("fee %s = %v, %v", text, money, ok)
		}
	}
	if money, ok := okxFee("-9223372036854775808", "USDT"); ok || money != nil {
		t.Fatalf("unrepresentable fee magnitude = %v, %v", money, ok)
	}
}
