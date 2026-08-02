package ledger

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
)

func d(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

func TestFromFillBuyDoubleEntry(t *testing.T) {
	fill := &orderpb.Fill{
		FillId: "F1", OrderId: "O1", InstrumentId: "AAPL", Side: orderpb.Side_SIDE_BUY,
		Quantity:   d(100, 0),
		Price:      d(150, 0),
		Fee:        &commonpb.Money{Amount: d(5, 0), CurrencyCode: "USD"},
		ExecutedAt: timestamppb.New(day(2)),
	}
	e, err := FromFill("PF", fill, "USD", day(2))
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	if e.Quantity.Cmp(big.NewRat(100, 1)) != 0 {
		t.Fatalf("buy qty: %s", e.Quantity.RatString())
	}
	// cash = -(100*150) - 5 = -15005
	if e.Cash.Cmp(big.NewRat(-15005, 1)) != 0 {
		t.Fatalf("buy cash: want -15005 got %s", e.Cash.RatString())
	}
	if e.EntryID != "fill:F1" {
		t.Fatalf("entry id: %s", e.EntryID)
	}
}

func TestFromFillSellDoubleEntry(t *testing.T) {
	fill := &orderpb.Fill{
		FillId: "F2", InstrumentId: "AAPL", Side: orderpb.Side_SIDE_SELL,
		Quantity: d(40, 0), Price: d(160, 0),
		Fee:        &commonpb.Money{Amount: d(2, 0), CurrencyCode: "USD"},
		ExecutedAt: timestamppb.New(day(3)),
	}
	e, err := FromFill("PF", fill, "USD", day(3))
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if e.Quantity.Cmp(big.NewRat(-40, 1)) != 0 {
		t.Fatalf("sell qty: %s", e.Quantity.RatString())
	}
	// cash = +(40*160) - 2 = 6398
	if e.Cash.Cmp(big.NewRat(6398, 1)) != 0 {
		t.Fatalf("sell cash: want 6398 got %s", e.Cash.RatString())
	}
}

// #221: OKX charges a spot BUY's fee in the BASE asset. Netting 0.0008 BTC off a
// USD cash leg books +1.0 BTC (the account received 0.9992) and −50000.0008 USD
// (exactly 50000.00 moved) — $40 of phantom NAV per BTC, permanent because the
// journal is append-only. The fee's currency is on the wire; it must be read.
func TestFromFillRefusesFeeInAnotherCurrency(t *testing.T) {
	fill := &orderpb.Fill{
		FillId: "F3", InstrumentId: "BTC-USDT", Side: orderpb.Side_SIDE_BUY,
		Quantity: d(1, 0), Price: d(50000, 0),
		Fee:        &commonpb.Money{Amount: d(8, -4), CurrencyCode: "BTC"},
		ExecutedAt: timestamppb.New(day(4)),
	}
	e, err := FromFill("PF", fill, "USD", day(4))
	if err == nil {
		t.Fatalf("a BTC fee on a USD book must be refused, got cash=%s", e.Cash.RatString())
	}
	if !errors.Is(err, dec.ErrCurrencyMismatch) {
		t.Fatalf("want ErrCurrencyMismatch, got %v", err)
	}
	for _, want := range []string{"F3", "0.0008", `"BTC"`, `"USD"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must name %s so an operator can act on it: %v", want, err)
		}
	}
}

// A NON-ZERO fee with no currency stamp is refused too: "nobody stamped this" and
// "stamped with the cash currency" must not look the same (#221).
func TestFromFillRefusesUnstampedFee(t *testing.T) {
	fill := &orderpb.Fill{
		FillId: "F4", InstrumentId: "AAPL", Side: orderpb.Side_SIDE_BUY,
		Quantity: d(10, 0), Price: d(100, 0),
		Fee: &commonpb.Money{Amount: d(5, 0)}, // currency_code unset
	}
	if _, err := FromFill("PF", fill, "USD", day(4)); !errors.Is(err, dec.ErrCurrencyMismatch) {
		t.Fatalf("unstamped fee must be refused, got %v", err)
	}
}

// A fill with NO fee has no fee currency to disagree with, whatever the stamp
// says — the zero-fee path (every sim/FIX venue) must not start DLQing.
func TestFromFillZeroFeeIsCurrencyAgnostic(t *testing.T) {
	for name, fee := range map[string]*commonpb.Money{
		"absent":      nil,
		"zero in BTC": {Amount: d(0, 0), CurrencyCode: "BTC"},
	} {
		t.Run(name, func(t *testing.T) {
			fill := &orderpb.Fill{
				FillId: "F5", InstrumentId: "AAPL", Side: orderpb.Side_SIDE_BUY,
				Quantity: d(10, 0), Price: d(100, 0), Fee: fee,
			}
			e, err := FromFill("PF", fill, "USD", day(4))
			if err != nil {
				t.Fatalf("zero fee must post: %v", err)
			}
			if e.Cash.Cmp(big.NewRat(-1000, 1)) != 0 {
				t.Fatalf("cash: want -1000 got %s", e.Cash.RatString())
			}
		})
	}
}
