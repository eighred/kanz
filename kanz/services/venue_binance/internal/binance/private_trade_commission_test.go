package binance

import (
	"testing"

	"github.com/eighred/kanz/internal/dec"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

func TestPrivateTradeRequiresReportedCommission(t *testing.T) {
	const trade = `{"e":"executionReport","s":"BTCUSDT","c":"o1","S":"BUY","x":"TRADE","X":"FILLED","l":"1","L":"50000","z":"1","q":"1","t":0,"T":1700000000000`
	for _, tc := range []struct{ name, fields, expected string }{
		{"unreported both", "", ""},
		{"unreported amount", `,"N":"USDT"`, ""},
		{"null amount", `,"n":null,"N":"USDT"`, ""},
		{"empty amount", `,"n":"","N":"USDT"`, ""},
		{"unreported asset", `,"n":"0.1"`, ""},
		{"null asset on explicit zero", `,"n":"0","N":null`, ""},
		{"blank asset", `,"n":"0.1","N":" "`, ""},
		{"reported zero", `,"n":"0.000","N":"USDT"`, "0"},
		{"reported fee", `,"n":"0.1","N":"USDT"`, "0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := newFakeOrders("o1")
			published := &reconCapture{}
			ing := ingesterOverView(nil, published, view)
			if err := ing.handle(t.Context(), []byte(trade+tc.fields+"}")); err != nil {
				t.Fatal(err)
			}
			st := view.get(t, "o1")
			if tc.expected == "" {
				if len(published.events) != 0 || st.GetQuarantine() == nil || dec.FromProto(st.GetFilledQuantity()).Sign() != 0 || ing.resolvedAReport() {
					t.Fatal("unproven commission was published or advanced the order", st)
				}
				return
			}
			if len(published.events) != 1 || st.GetQuarantine() != nil || !ing.resolvedAReport() {
				t.Fatal("complete trade was not published", st)
			}
			receipt := published.events[0].Payload.(*orderpb.OrderFilled).GetFill()
			if receipt.GetFee().GetAmount() == nil || dec.Cmp(receipt.Fee.Amount, bdec(tc.expected)) != 0 || receipt.Fee.CurrencyCode != "USDT" {
				t.Fatal("reported commission lost", receipt.Fee)
			}
		})
	}
}
