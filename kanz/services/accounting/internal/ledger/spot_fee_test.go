package ledger

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/fillfact"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func spotExecution(venue, id, symbol string, side orderpb.Side, qty, price, fee *commonpb.Decimal, currency string) *orderpb.Fill {
	return &orderpb.Fill{
		FillId: id, OrderId: "order-" + id, Venue: venue, VenueAccountId: "account-" + venue,
		VenueExecutionId: id, InstrumentId: "BTC-USDT", SpotVenueSymbol: symbol,
		Side: side, Quantity: qty, Price: price,
		Fee: &commonpb.Money{Amount: fee, CurrencyCode: currency}, ExecutedAt: timestamppb.New(day(1)),
	}
}

func TestTwoVenueNativeSettlementAndRevision(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store Store = NewMemoryStore()
			if backend == "postgres" {
				store = NewPostgres(newPool(t))
			}
			ctx := context.Background()
			post := func(f *orderpb.Fill, known time.Time) {
				t.Helper()
				e, err := FromFill("spot-fund", f, "USD", known)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Append(ctx, e, nil); err != nil {
					t.Fatal(err)
				}
			}
			open := spotExecution("BINANCE", "start", "BTCUSDT", orderpb.Side_SIDE_BUY, d(1, 0), d(50000, 0), d(0, 0), "USDT")
			buy := spotExecution("OKX", "buy", "BTC-USDT", orderpb.Side_SIDE_BUY, d(1, 0), d(50000, 0), d(8, -4), "BTC")
			sell := spotExecution("BINANCE", "sell", "BTCUSDT", orderpb.Side_SIDE_SELL, d(9992, -4), d(50030, 0), d(25, 0), "USDT")
			post(open, day(1))
			post(buy, day(2))
			post(sell, day(3))
			post(buy, day(2))
			read := func() []*Event {
				j, err := store.Journal(ctx, "spot-fund")
				if err != nil {
					t.Fatal(err)
				}
				return j
			}
			j := read()
			assertRat := func(label string, got *big.Rat, want string) {
				t.Helper()
				expected, ok := new(big.Rat).SetString(want)
				if !ok || got == nil || got.Cmp(expected) != 0 {
					t.Fatalf("%s got %v want %s", label, got, want)
				}
			}
			book := Replay("spot-fund", j)
			assertRat("total inventory", book.Positions["BTC-USDT"].Qty, "1")
			assertRat("settled cash including opening purchase", book.SettledCash["USDT"], "-50035.024")
			if _, ok := book.Cash["USD"]; ok {
				t.Fatal("USDT booked as dollars")
			}
			assertRat("gross average basis", book.Positions["BTC-USDT"].AvgCost, "50000")
			assertRat("gross realized", book.Positions["BTC-USDT"].Realized, "29.976")
			if err := store.SaveSnapshot(ctx, book.Snapshot(day(3))); err != nil {
				t.Fatal(err)
			}
			cash := VenueAccountCash(j)
			assertRat("OKX funded quote", cash["account-OKX"]["USDT"], "-50000")
			assertRat("Binance net quote including opening purchase", cash["account-BINANCE"]["USDT"], "-35.024")
			trades, n := executionsOf(j)
			if n != 0 || len(trades) != 3 {
				t.Fatal(trades, n)
			}
			for _, x := range trades {
				if x.Ref == "buy" {
					assertRat("gross confirmed trade", x.Quantity, "1")
				}
			}
			rebate := approvedLedgerFee(t, buy, "spot-fee-revision", -1)
			rebate.Fee.Amount = d(-2, -4)
			post(rebate, day(4))
			post(rebate, day(4))
			post(buy, day(5))
			j = read()
			if len(j) != 4 {
				t.Fatalf("expected one revision, journal has %d", len(j))
			}
			book = ReplayAsOf("spot-fund", j, day(5), day(3))
			assertRat("before revision", book.SettledPositions["BTC-USDT"].Qty, "1")
			book = Replay("spot-fund", j)
			assertRat("base rebate", book.SettledPositions["BTC-USDT"].Qty, "1.001")
			current, _, err := MaterializeCurrent(ctx, store, "spot-fund")
			if err != nil {
				t.Fatal(err)
			}
			assertRat("checkpoint and revised journal", current.SettledPositions["BTC-USDT"].Qty, "1.001")
			assertRat("restated cash", book.SettledCash["USDT"], "-50035.024")
			for _, e := range j {
				if e.Type == EntryFee {
					if e.SourceRef != "fill:"+fillfact.ExecutionKey(buy) || e.Cash != nil {
						t.Fatal(e)
					}
					assertRat("native delta", e.Quantity, "0.001")
				}
			}
		})
	}
}

func TestBaseFeeRestatesBasisBeforeOtherExecutionsAtSameTime(t *testing.T) {
	first := spotExecution("OKX", "a", "BTC-USDT", orderpb.Side_SIDE_BUY, d(1, 0), d(10, 0), d(1, -1), "BTC")
	entry := func(f *orderpb.Fill, known int) *Event {
		t.Helper()
		e, err := FromFill("fund", f, "USDT", day(known))
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	a := entry(first, 1)
	b := entry(spotExecution("OKX", "b", "BTC-USDT", orderpb.Side_SIDE_BUY, d(1, 0), d(30, 0), d(0, 0), "USDT"), 2)
	c := entry(spotExecution("OKX", "c", "BTC-USDT", orderpb.Side_SIDE_SELL, d(1, 0), d(40, 0), d(0, 0), "USDT"), 3)
	revised := approvedLedgerFee(t, first, "basis", -1)
	revised.Fee.Amount = d(-1, -1)
	delta, err := fillfact.ApprovedFeeDelta(first, revised)
	if err != nil {
		t.Fatal(err)
	}
	fee := feeAdjustment(entry(revised, 4), revised, delta)
	j := []*Event{fee, c, a, fee, b}
	before := ReplayAsOf("fund", j, time.Time{}, day(3)).Positions["BTC-USDT"]
	after := Replay("fund", j).Positions["BTC-USDT"]
	if before.AvgCost.Cmp(big.NewRat(390, 19)) != 0 || before.Realized.Cmp(big.NewRat(370, 19)) != 0 ||
		after.Qty.Cmp(big.NewRat(11, 10)) != 0 || after.AvgCost.Cmp(big.NewRat(410, 21)) != 0 || after.Realized.Cmp(big.NewRat(430, 21)) != 0 {
		t.Fatalf("basis before %+v after %+v", before, after)
	}
}
