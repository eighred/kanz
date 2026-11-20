package binance

import (
	"fmt"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"strings"
	"testing"
)

func TestHistoryRequiresTradeIdentitySideAndTimeForEveryMatch(t *testing.T) {
	const order = `{"symbol":"BTCUSDT","clientOrderId":"o1","orderId":7,"status":"FILLED","executedQty":"1"}`
	const trades = `[{"symbol":"BTCUSDT","orderId":7,"isBuyer":true,"id":0,"qty":"0.4","price":"500","time":1750000000999},{"symbol":"BTCUSDT","orderId":7,"isBuyer":true,"id":42,"qty":"0.6","price":"501","time":1750000001001}]`
	for _, tc := range []struct{ name, find, replace string }{
		{"missing trade zero", `"id":0,`, ""},
		{"null trade zero", `"id":0`, `"id":null`},
		{"ambiguous trade zero", `"id":0`, `"id":8,"id":0`},
		{"missing second time", `,"time":1750000001001`, ""},
		{"zero time", `"time":1750000001001`, `"time":0`},
		{"null time", `"time":1750000001001`, `"time":null`},
		{"nonrepresentable time", `"time":1750000001001`, `"time":253402300800000`},
		{"ambiguous time", `"time":1750000001001`, `"time":101,"time":1750000001001`},
		{"missing buy or sell", `"isBuyer":true,`, ""},
		{"other side", `"isBuyer":true`, `"isBuyer":false`},
	} {
		for _, status := range []string{"FILLED", "PARTIALLY_FILLED", "CANCELED"} {
			t.Run(tc.name+"/"+status, func(t *testing.T) {
				f := newFakeBinance(t)
				f.queryBody = strings.Replace(order, "FILLED", status, 1)
				f.myTradesBody = strings.Replace(trades, tc.find, tc.replace, 1)
				got, err := venueOverFake(f).QueryOrder(t.Context(), queryOrderState("o1"))
				if err == nil || len(got.Fills) != 0 || got.State == OrderViewUnknown || f.posts != 0 {
					t.Fatalf("ambiguous history became a financial outcome: %+v %v", got, err)
				}
			})
		}
	}
	for _, sell := range []bool{false, true} {
		t.Run(fmt.Sprintf("two valid matches selling=%t", sell), func(t *testing.T) {
			f := newFakeBinance(t)
			f.queryBody = order
			f.myTradesBody = trades
			st := queryOrderState("o1")
			if sell {
				st.Side = orderpb.Side_SIDE_SELL
				f.myTradesBody = strings.ReplaceAll(trades, `"isBuyer":true`, `"isBuyer":false`)
			}
			got, err := venueOverFake(f).QueryOrder(t.Context(), st)
			if err != nil || len(got.Fills) != 2 || got.Fills[0].GetVenueExecutionId() != "0" || got.Fills[1].GetVenueExecutionId() != "42" || got.Fills[0].GetExecutedAt().AsTime().UnixMilli() != 1750000000999 || got.Fills[1].GetExecutedAt().AsTime().UnixMilli() != 1750000001001 || got.Fills[0].GetSide() != st.Side || f.posts != 0 {
				t.Fatalf("individual execution attribution: %v %v", got, err)
			}
		})
	}
	f := newFakeBinance(t)
	f.queryBody = strings.Replace(order, "o1", "foreign", 1)
	f.myTradesBody = trades
	if got, err := venueOverFake(f).QueryOrder(t.Context(), queryOrderState("o1")); err == nil || len(got.Fills) != 0 || f.myTradesCalls != 0 {
		t.Fatalf("unbound client order exposed trade history: %+v %v", got, err)
	}
}

func TestPrivateStreamRefusesOutOfRangeMatchTime(t *testing.T) {
	cap := &reconCapture{}
	ing := ingesterOver(nil, cap)
	frame := `{"e":"executionReport","E":1700000000000,"s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"FILLED","l":"1","L":"500","z":"1","t":0,"T":253402300800000}`
	if err := ing.handle(t.Context(), []byte(frame)); err == nil || len(cap.events) != 0 || ing.resolvedAReport() {
		t.Fatal("out-of-range execution time produced a fill")
	}
}
