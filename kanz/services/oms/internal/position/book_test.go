package position

import (
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/kanz-eng/kanz/internal/dec"
)

func d(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

func fill(side orderpb.Side, qty, price *commonpb.Decimal) *orderpb.Fill {
	return &orderpb.Fill{
		InstrumentId: "AAPL",
		Side:         side,
		Quantity:     qty,
		Price:        price,
		ExecutedAt:   timestamppb.New(time.Unix(0, 0)),
	}
}

func TestBook_BuyThenReduce_RealizesPnL(t *testing.T) {
	b := NewBook("USD")
	b.Apply("pf1", fill(orderpb.Side_SIDE_BUY, d(100, 0), d(10, 0)), time.Unix(0, 0))
	st := b.Apply("pf1", fill(orderpb.Side_SIDE_SELL, d(40, 0), d(12, 0)), time.Unix(0, 0))

	if dec.Cmp(st.GetQuantity(), d(60, 0)) != 0 {
		t.Fatalf("qty = %v, want 60", st.GetQuantity())
	}
	if dec.Cmp(st.GetAveragePrice(), d(10, 0)) != 0 {
		t.Fatalf("avg = %v, want 10", st.GetAveragePrice())
	}
	// realized = 40*(12-10) = 80
	if dec.Cmp(st.GetRealizedPnl().GetAmount(), d(80, 0)) != 0 {
		t.Fatalf("realized = %v, want 80", st.GetRealizedPnl().GetAmount())
	}
	// market_value = 12*60 = 720
	if dec.Cmp(st.GetMarketValue().GetAmount(), d(720, 0)) != 0 {
		t.Fatalf("mv = %v, want 720", st.GetMarketValue().GetAmount())
	}
	if st.GetMarketValue().GetCurrencyCode() != "USD" {
		t.Fatalf("currency = %q, want USD", st.GetMarketValue().GetCurrencyCode())
	}
}

func TestBook_CrossesZero_OpensNewLot(t *testing.T) {
	b := NewBook("USD")
	b.Apply("pf1", fill(orderpb.Side_SIDE_BUY, d(100, 0), d(10, 0)), time.Unix(0, 0))
	st := b.Apply("pf1", fill(orderpb.Side_SIDE_SELL, d(150, 0), d(12, 0)), time.Unix(0, 0))

	// closed 100 long @ +2 ⇒ realized 200; remaining 50 short opened at 12.
	if dec.Cmp(st.GetQuantity(), d(-50, 0)) != 0 {
		t.Fatalf("qty = %v, want -50", st.GetQuantity())
	}
	if dec.Cmp(st.GetAveragePrice(), d(12, 0)) != 0 {
		t.Fatalf("avg = %v, want 12", st.GetAveragePrice())
	}
	if dec.Cmp(st.GetRealizedPnl().GetAmount(), d(200, 0)) != 0 {
		t.Fatalf("realized = %v, want 200", st.GetRealizedPnl().GetAmount())
	}
}
