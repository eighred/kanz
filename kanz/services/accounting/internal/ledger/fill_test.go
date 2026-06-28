package ledger

import (
	"math/big"
	"testing"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
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
	e := FromFill("PF", fill, "USD", day(2))
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
	e := FromFill("PF", fill, "USD", day(3))
	if e.Quantity.Cmp(big.NewRat(-40, 1)) != 0 {
		t.Fatalf("sell qty: %s", e.Quantity.RatString())
	}
	// cash = +(40*160) - 2 = 6398
	if e.Cash.Cmp(big.NewRat(6398, 1)) != 0 {
		t.Fatalf("sell cash: want 6398 got %s", e.Cash.RatString())
	}
}
