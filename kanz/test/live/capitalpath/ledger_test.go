package main

import (
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestMatchLedgerRowRequiresExactFillIdentityAndEconomics(t *testing.T) {
	executed := time.Unix(1_700_000_000, 123_000_000).UTC()
	fill := &orderpb.Fill{
		FillId: "fill-7", OrderId: "order-1", InstrumentId: "BTC-USDT", Side: orderpb.Side_SIDE_BUY,
		Quantity: &commonpb.Decimal{Coefficient: 25, Exponent: -4},
		Price:    &commonpb.Decimal{Coefficient: 500001, Exponent: -1},
		Venue:    "XOKX", VenueAccountId: "OKX-DEMO-1", ExecutedAt: timestamppb.New(executed),
	}
	want := ledgerRow{
		entryID: "fill:fill-7", portfolioID: "PF", venueAccountID: "OKX-DEMO-1",
		entryType: 1, instrumentID: "BTC-USDT", quantity: "1/400", price: "500001/10",
		effective: executed, sourceRef: "fill-7",
	}
	if err := matchLedgerRow(config{portfolio: "PF", account: "OKX-DEMO-1", instrument: "BTC-USDT"}, fill, want); err != nil {
		t.Fatalf("matchLedgerRow: %v", err)
	}

	mutations := []struct {
		name string
		edit func(*ledgerRow)
		want string
	}{
		{"wrong account", func(r *ledgerRow) { r.venueAccountID = "OTHER" }, "venue account"},
		{"wrong quantity", func(r *ledgerRow) { r.quantity = "1/399" }, "quantity"},
		{"wrong price", func(r *ledgerRow) { r.price = "50000" }, "price"},
		{"wrong source", func(r *ledgerRow) { r.sourceRef = "another" }, "source"},
		{"wrong effective time", func(r *ledgerRow) { r.effective = r.effective.Add(time.Second) }, "effective"},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			got := want
			tc.edit(&got)
			err := matchLedgerRow(config{portfolio: "PF", account: "OKX-DEMO-1", instrument: "BTC-USDT"}, fill, got)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestMatchLedgerRowUsesSignedSellQuantity(t *testing.T) {
	fill := &orderpb.Fill{
		FillId: "fill-sell", InstrumentId: "BTC-USDT", Side: orderpb.Side_SIDE_SELL,
		Quantity: &commonpb.Decimal{Coefficient: 2}, Price: &commonpb.Decimal{Coefficient: 10},
		Venue: "XOKX", VenueAccountId: "A", ExecutedAt: timestamppb.New(time.Unix(100, 0)),
	}
	row := ledgerRow{entryID: "fill:fill-sell", portfolioID: "PF", venueAccountID: "A", entryType: 1,
		instrumentID: "BTC-USDT", quantity: "-2", price: "10", effective: time.Unix(100, 0), sourceRef: "fill-sell"}
	if err := matchLedgerRow(config{portfolio: "PF", account: "A", instrument: "BTC-USDT"}, fill, row); err != nil {
		t.Fatal(err)
	}
}
