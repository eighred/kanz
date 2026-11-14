package main

import (
	"fmt"
	"math"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// Each sequential input comes from integer units of 1e-9, not from a Go or Rust
// result. Matching a shared rounded implementation cannot satisfy this corpus.
func quantityPrecisionCases() []sample {
	var out []sample
	for _, exponent := range []int32{-61, -60, 60, 61} {
		st := baseState()
		st.OrderedQuantity = decimal(2, exponent)
		st.LeavesQuantity = decimal(2, exponent)
		q := rq("ApplyFill", st)
		q.Fill = wire(&orderpb.Fill{Quantity: decimal(1, exponent), Price: decimal(1, 0)})
		s := sample{name: fmt.Sprintf("exact-quantity-domain-%d", exponent), request: q}
		if exponent == -61 || exponent == 61 {
			s.numerical = true
		} else {
			s.want = changed(st, orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED)
			s.want.FilledQuantity = decimal(1, exponent)
			s.want.LeavesQuantity = decimal(1, exponent)
			s.want.AverageFillPrice = decimal(100000000, -8)
		}
		out = append(out, s)
	}
	for i := int64(0); i < 10; i++ {
		st := baseState()
		st.OrderedQuantity = decimal(1, -8)
		st.FilledQuantity = decimal(i, -9)
		st.LeavesQuantity = decimal(10-i, -9)
		st.AverageFillPrice = decimal(1, 0)
		want := changed(st, orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED)
		want.FilledQuantity = decimal(i+1, -9)
		if i == 9 {
			want.FilledQuantity = decimal(1, -8)
			want.Status = orderpb.OrderStatus_ORDER_STATUS_FILLED
		}
		want.LeavesQuantity = decimal(9-i, -9)
		if i == 9 {
			want.LeavesQuantity = decimal(0, 0)
		}
		want.AverageFillPrice = rounded(rational(decimal(1, 0)))
		q := rq("ApplyFill", st)
		q.Fill = wire(&orderpb.Fill{Quantity: decimal(1, -9), Price: decimal(1, 0)})
		out = append(out, sample{name: fmt.Sprintf("exact-quantity-sequence-%d", i+1), request: q, want: want})
	}
	for _, op := range []string{"ApplyFill", "Amend"} {
		st := baseState()
		st.OrderedQuantity = decimal(math.MaxInt64, 0)
		st.LeavesQuantity = st.OrderedQuantity
		q := rq(op, st)
		if op == "ApplyFill" {
			q.Fill = wire(&orderpb.Fill{Quantity: decimal(1, -9), Price: decimal(1, 0)})
		} else {
			st.FilledQuantity = decimal(1, -9)
			q.State = wire(st)
			q.Command = wire(&orderpb.AmendOrder{NewQuantity: st.OrderedQuantity})
		}
		out = append(out, sample{name: "exact-quantity-refusal-" + op, request: q, numerical: true})
	}
	st := baseState()
	st.OrderedQuantity = decimal(1, 19)
	st.FilledQuantity = decimal(9, 18)
	st.LeavesQuantity = decimal(1, 18)
	q := rq("ApplyFill", st)
	q.Fill = wire(&orderpb.Fill{Quantity: decimal(1, -9), Price: decimal(1, 0)})
	out = append(out, sample{name: "exact-quantity-refusal-filled-sum", request: q, numerical: true})
	for _, coefficient := range []int64{11, -1} {
		st := baseState()
		st.OrderedQuantity = decimal(1, -8)
		st.FilledQuantity = decimal(9, -9)
		st.LeavesQuantity = decimal(1, -9)
		q := rq("ApplyFill", st)
		q.Fill = wire(&orderpb.Fill{Quantity: decimal(coefficient, -10), Price: decimal(1, 0)})
		code := "OVERFILL"
		if coefficient < 0 {
			code = "INVALID_FILL"
		}
		out = append(out, sample{name: "exact-quantity-refusal-" + code, request: q, code: code})
	}
	return out
}
