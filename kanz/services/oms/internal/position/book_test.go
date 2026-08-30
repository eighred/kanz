package position

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
)

func d(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

// fill takes an EXPLICIT fill id because the book now folds each one exactly
// once (#818). This helper used to stamp "F-test" on every fill it built, so
// these two tests fed the book one fill_id twice and called it two trades —
// a sequence Postgres has always refused, expressed against a seam that
// accepted it. That is the divergence the issue is about, sitting in the
// package's own tests.
func fill(id string, side orderpb.Side, qty, price *commonpb.Decimal) *orderpb.Fill {
	return &orderpb.Fill{FillId: id,
		InstrumentId: "AAPL",
		Side:         side,
		Quantity:     qty,
		Price:        price,
		Venue:        "XBIN",
		ExecutedAt:   timestamppb.New(time.Unix(0, 0)),
	}
}

func TestBook_BuyThenReduce_RealizesPnL(t *testing.T) {
	b := NewBook("USD")
	_, _ = b.Apply(context.Background(), "pf1", fill("F-open", orderpb.Side_SIDE_BUY, d(100, 0), d(10, 0)), time.Unix(0, 0), nil)
	st, _ := b.Apply(context.Background(), "pf1", fill("F-reduce", orderpb.Side_SIDE_SELL, d(40, 0), d(12, 0)), time.Unix(0, 0), nil)

	if dec.Cmp(st.Aggregate.GetQuantity(), d(60, 0)) != 0 {
		t.Fatalf("qty = %v, want 60", st.Aggregate.GetQuantity())
	}
	if dec.Cmp(st.Aggregate.GetAveragePrice(), d(10, 0)) != 0 {
		t.Fatalf("avg = %v, want 10", st.Aggregate.GetAveragePrice())
	}
	// realized = 40*(12-10) = 80
	if dec.Cmp(st.Aggregate.GetRealizedPnl().GetAmount(), d(80, 0)) != 0 {
		t.Fatalf("realized = %v, want 80", st.Aggregate.GetRealizedPnl().GetAmount())
	}
	// market_value = 12*60 = 720
	if dec.Cmp(st.Aggregate.GetMarketValue().GetAmount(), d(720, 0)) != 0 {
		t.Fatalf("mv = %v, want 720", st.Aggregate.GetMarketValue().GetAmount())
	}
	if st.Aggregate.GetMarketValue().GetCurrencyCode() != "USD" {
		t.Fatalf("currency = %q, want USD", st.Aggregate.GetMarketValue().GetCurrencyCode())
	}
}

func TestBook_CrossesZero_OpensNewLot(t *testing.T) {
	b := NewBook("USD")
	_, _ = b.Apply(context.Background(), "pf1", fill("F-open", orderpb.Side_SIDE_BUY, d(100, 0), d(10, 0)), time.Unix(0, 0), nil)
	st, _ := b.Apply(context.Background(), "pf1", fill("F-cross", orderpb.Side_SIDE_SELL, d(150, 0), d(12, 0)), time.Unix(0, 0), nil)

	// closed 100 long @ +2 ⇒ realized 200; remaining 50 short opened at 12.
	if dec.Cmp(st.Aggregate.GetQuantity(), d(-50, 0)) != 0 {
		t.Fatalf("qty = %v, want -50", st.Aggregate.GetQuantity())
	}
	if dec.Cmp(st.Aggregate.GetAveragePrice(), d(12, 0)) != 0 {
		t.Fatalf("avg = %v, want 12", st.Aggregate.GetAveragePrice())
	}
	if dec.Cmp(st.Aggregate.GetRealizedPnl().GetAmount(), d(200, 0)) != 0 {
		t.Fatalf("realized = %v, want 200", st.Aggregate.GetRealizedPnl().GetAmount())
	}
}
