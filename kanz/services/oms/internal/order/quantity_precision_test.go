package order

import (
	"errors"
	"math/big"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
)

func TestFillQuantitiesConserveTenSubEightPlaceExecutions(t *testing.T) {
	st, err := Accept(limitOrder(d(1, -8), d(1, 0)), t0)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 10; i++ {
		before := proto.Clone(st)
		next, err := ApplyFill(st, &orderpb.Fill{Quantity: d(1, -9), Price: d(1, 0)}, t0)
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(st, before) {
			t.Fatal("fold mutated its input")
		}
		if dec.FromProto(next.FilledQuantity).Cmp(big.NewRat(i, 1000000000)) != 0 ||
			dec.FromProto(next.LeavesQuantity).Cmp(big.NewRat(10-i, 1000000000)) != 0 {
			t.Fatalf("after %d fills: filled=%v leaves=%v", i, next.FilledQuantity, next.LeavesQuantity)
		}
		want := orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED
		if i == 10 {
			want = orderpb.OrderStatus_ORDER_STATUS_FILLED
		}
		if next.Status != want {
			t.Fatalf("after %d fills: status=%s, want %s", i, next.Status, want)
		}
		st = next
	}
}

func TestQuantityPrecisionRefusesInvalidInputsBeforeArithmetic(t *testing.T) {
	for _, value := range []*commonpb.Decimal{d(1, 2147483647), d(1, -2147483648)} {
		st, err := Accept(limitOrder(d(2, 0), d(1, 0)), t0)
		if err != nil {
			t.Fatal(err)
		}
		if next, err := ApplyFill(st, &orderpb.Fill{Quantity: value, Price: d(1, 0)}, t0); err == nil || next != nil {
			t.Fatal("unbounded fill quantity accepted")
		}
		if next, err := Amend(st, &orderpb.AmendOrder{NewQuantity: value}, t0); err == nil || next != nil {
			t.Fatal("unbounded amendment quantity accepted")
		}
	}
	st, _ := Accept(limitOrder(d(2, 0), d(1, 0)), t0)
	st.FilledQuantity = d(-1, -9)
	if next, err := ApplyFill(st, &orderpb.Fill{Quantity: d(2, -9), Price: d(1, 0)}, t0); err == nil || next != nil {
		t.Fatal("negative prior filled quantity was silently healed")
	}
}

func TestAmendPreservesSubEightPlaceLeaves(t *testing.T) {
	st, err := Accept(limitOrder(d(2, 0), d(1, 0)), t0)
	if err != nil {
		t.Fatal(err)
	}
	st, err = ApplyFill(st, &orderpb.Fill{Quantity: d(1, 0), Price: d(1, 0)}, t0)
	if err != nil {
		t.Fatal(err)
	}
	next, err := Amend(st, &orderpb.AmendOrder{NewQuantity: d(1000000009, -9)}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if dec.FromProto(next.LeavesQuantity).Cmp(big.NewRat(9, 1000000000)) != 0 {
		t.Fatalf("amended leaves=%v, want 9e-9", next.LeavesQuantity)
	}
	_, err = ApplyFill(next, &orderpb.Fill{Quantity: d(91, -10), Price: d(1, 0)}, t0)
	var rejected *RejectError
	if !errors.As(err, &rejected) || rejected.Code != "OVERFILL" {
		t.Fatalf("sub-eight-place overfill: %v", err)
	}
}

func TestQuantityPrecisionRefusesUnrepresentableResidual(t *testing.T) {
	st, err := Accept(limitOrder(d(9223372036854775807, 0), d(1, 0)), t0)
	if err != nil {
		t.Fatal(err)
	}
	before := proto.Clone(st)
	if next, err := ApplyFill(st, &orderpb.Fill{Quantity: d(1, -9), Price: d(1, 0)}, t0); err == nil || next != nil {
		t.Fatalf("unrepresentable leaves were rounded: state=%v err=%v", next, err)
	}
	if !proto.Equal(st, before) {
		t.Fatal("refused fold mutated state")
	}
	st.FilledQuantity = d(1, -9)
	before = proto.Clone(st)
	if next, err := Amend(st, &orderpb.AmendOrder{NewQuantity: d(9223372036854775807, 0)}, t0); err == nil || next != nil {
		t.Fatalf("unrepresentable amended leaves were rounded: state=%v err=%v", next, err)
	}
	if !proto.Equal(st, before) {
		t.Fatal("refused amendment mutated state")
	}
}
