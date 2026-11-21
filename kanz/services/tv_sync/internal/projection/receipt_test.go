package projection

import (
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	tvsyncpb "github.com/eighred/kanz/kanz-schemas-go/tvsync/v1"
	"google.golang.org/protobuf/proto"
)

func TestNativeBrokerExecutionReceiptsSurviveCheckpoint(t *testing.T) {
	log := newMemLog()
	p := New(time.Now, nil, WithLog(log, testTenant))
	charge := func(n int64, exp int32, unit string) *commonpb.Money {
		return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: n, Exponent: exp}, CurrencyCode: unit}
	}
	cases := []struct {
		id, venue, symbol              string
		side                           orderpb.Side
		qty                            *commonpb.Decimal
		price                          int64
		fee                            *commonpb.Money
		wantFee, unit, inventory, cash string
	}{
		{"buy", "OKX", "BTC-USDT", orderpb.Side_SIDE_BUY, d(1), 50000, charge(8, -4, "BTC"), "0.0008", "BTC", "0.9992", "-50000"},
		{"sell", "BINANCE", "BTCUSDT", orderpb.Side_SIDE_SELL, &commonpb.Decimal{Coefficient: 9992, Exponent: -4}, 50030, charge(25, 0, "USDT"), "25", "USDT", "-0.9992", "49964.976"},
		{"rebate", "OKX", "BTC-USDT", orderpb.Side_SIDE_BUY, d(1), 50, charge(-2, -10, "BTC"), "-0.0000000002", "BTC", "1.0000000002", "-50"},
		{"zero", "OKX", "BTC-USDT", orderpb.Side_SIDE_SELL, &commonpb.Decimal{Coefficient: 1, Exponent: -10}, 50, charge(0, 0, "USDT"), "0", "USDT", "-0.0000000001", "0.000000005"},
		{"missing", "OKX", "BTC-USDT", orderpb.Side_SIDE_BUY, d(1), 50, nil, "", "", "", ""},
		{"other", "BINANCE", "BTCUSDT", orderpb.Side_SIDE_BUY, d(1), 50, charge(1, -3, "BNB"), "0.001", "BNB", "", ""},
	}
	for _, c := range cases {
		ev := filledOrder(c.id, "fund", "BTC-USDT", c.side, 1, c.price)
		ev.Fill.Venue, ev.Fill.SpotVenueSymbol, ev.Fill.Fee, ev.Fill.Quantity = c.venue, c.symbol, c.fee, c.qty
		ev.State.OrderedQuantity, ev.State.FilledQuantity = c.qty, c.qty
		deliver(t, p, c.id, evtFilled, ev)
	}
	check := func(p *Projection) {
		t.Helper()
		rows, err := p.Executions(testTenant, "fund", "BTC-USDT", time.Time{})
		if err != nil || len(rows) != len(cases) {
			t.Fatalf("receipts %v %v", rows, err)
		}
		for i, r := range rows {
			c := cases[i]
			if r.OrderID != c.id || r.BaseCurrency != "BTC" || r.QuoteCurrency != "USDT" || r.Fee != c.wantFee || r.FeeCurrency != c.unit || r.InventoryChange != c.inventory || r.CashChange != c.cash {
				t.Fatalf("receipt %s: %+v", c.id, r)
			}
		}
		if rows[3].Qty != "0.0000000001" {
			t.Fatalf("rounded execution %+v", rows[3])
		}
	}
	check(p)
	if err := p.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	restore := func() *Projection {
		t.Helper()
		r := New(time.Now, nil, WithLog(log, testTenant))
		if err := r.Rehydrate(t.Context()); err != nil {
			t.Fatal(err)
		}
		return r
	}
	check(restore())
	// A historical checkpoint can report a nonzero fee with an unknown
	// denomination. Its zero placeholder cannot establish an explicit zero.
	var old tvsyncpb.Checkpoint
	if err := proto.Unmarshal(log.cp, &old); err != nil {
		t.Fatal(err)
	}
	for _, row := range old.Accounts[0].Execs {
		row.SourceFill = nil
	}
	log.cp = mustMarshal(t, &old)
	rows, err := restore().Executions(testTenant, "fund", "", time.Time{})
	if err != nil || rows[0].Fee != "0.0008" || rows[0].FeeCurrency != "" || rows[0].InventoryChange != "" || rows[3].Fee != "" {
		t.Fatalf("legacy receipts %+v %v", rows, err)
	}
}
