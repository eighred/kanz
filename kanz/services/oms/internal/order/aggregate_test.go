package order

import (
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

func limitOrder(qty *commonpb.Decimal, price *commonpb.Decimal) *orderpb.SubmitOrder {
	return &orderpb.SubmitOrder{
		OrderId:      "o1",
		PortfolioId:  "pf1",
		InstrumentId: "AAPL",
		Side:         orderpb.Side_SIDE_BUY,
		Quantity:     qty,
		OrderType:    orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:   price,
		TimeInForce:  orderpb.TimeInForce_TIME_IN_FORCE_DAY,
	}
}

var t0 = time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

func TestAccept_Valid(t *testing.T) {
	st, err := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW {
		t.Fatalf("status = %v, want PENDING_NEW", st.GetStatus())
	}
	if dec.Cmp(st.GetLeavesQuantity(), d(100, 0)) != 0 {
		t.Fatalf("leaves = %v, want 100", st.GetLeavesQuantity())
	}
	if !dec.IsZero(st.GetFilledQuantity()) {
		t.Fatalf("filled = %v, want 0", st.GetFilledQuantity())
	}
}

func TestAccept_Rejections(t *testing.T) {
	cases := map[string]func(*orderpb.SubmitOrder){
		"INVALID_ORDER":    func(c *orderpb.SubmitOrder) { c.OrderId = "" },
		"INVALID_QUANTITY": func(c *orderpb.SubmitOrder) { c.Quantity = d(0, 0) },
		"INVALID_PRICE":    func(c *orderpb.SubmitOrder) { c.LimitPrice = nil },
	}
	for wantCode, mutate := range cases {
		cmd := limitOrder(d(100, 0), d(1025, -2))
		mutate(cmd)
		_, err := Accept(cmd, t0)
		re, ok := err.(*RejectError)
		if !ok {
			t.Fatalf("%s: err = %v, want *RejectError", wantCode, err)
		}
		if re.Code != wantCode {
			t.Fatalf("code = %q, want %q", re.Code, wantCode)
		}
	}
}

func TestApplyFill_PartialThenFull(t *testing.T) {
	st, _ := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	st, _ = Route(st, t0)

	fill1 := &orderpb.Fill{Quantity: d(40, 0), Price: d(1000, -2), ExecutedAt: timestamppb.New(t0)}
	st, err := ApplyFill(st, fill1, t0)
	if err != nil {
		t.Fatalf("fill1: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		t.Fatalf("status = %v, want PARTIALLY_FILLED", st.GetStatus())
	}
	if dec.Cmp(st.GetLeavesQuantity(), d(60, 0)) != 0 {
		t.Fatalf("leaves = %v, want 60", st.GetLeavesQuantity())
	}

	fill2 := &orderpb.Fill{Quantity: d(60, 0), Price: d(1050, -2), ExecutedAt: timestamppb.New(t0)}
	st, err = ApplyFill(st, fill2, t0)
	if err != nil {
		t.Fatalf("fill2: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status = %v, want FILLED", st.GetStatus())
	}
	if !dec.IsZero(st.GetLeavesQuantity()) {
		t.Fatalf("leaves = %v, want 0", st.GetLeavesQuantity())
	}
	// VWAP = (40*10.00 + 60*10.50)/100 = 10.30
	if dec.Cmp(st.GetAverageFillPrice(), d(1030, -2)) != 0 {
		t.Fatalf("avg = %v, want 10.30", st.GetAverageFillPrice())
	}
}

func TestApplyFill_Overfill(t *testing.T) {
	st, _ := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	_, err := ApplyFill(st, &orderpb.Fill{Quantity: d(101, 0), Price: d(1000, -2), ExecutedAt: timestamppb.New(t0)}, t0)
	re, ok := err.(*RejectError)
	if !ok || re.Code != "OVERFILL" {
		t.Fatalf("err = %v, want OVERFILL", err)
	}
}

func TestCancel_And_TerminalGuard(t *testing.T) {
	st, _ := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	cancelled, qty, err := Cancel(st, t0)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("status = %v, want CANCELLED", cancelled.GetStatus())
	}
	if dec.Cmp(qty, d(100, 0)) != 0 {
		t.Fatalf("cancelled qty = %v, want 100", qty)
	}
	if _, _, err := Cancel(cancelled, t0); err == nil {
		t.Fatal("cancel of terminal order: want error")
	}
}

func TestAmend_BelowFilledRejected(t *testing.T) {
	st, _ := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	st, _ = ApplyFill(st, &orderpb.Fill{Quantity: d(50, 0), Price: d(1000, -2), ExecutedAt: timestamppb.New(t0)}, t0)
	_, err := Amend(st, &orderpb.AmendOrder{OrderId: "o1", NewQuantity: d(40, 0)}, t0)
	re, ok := err.(*RejectError)
	if !ok || re.Code != "INVALID_QUANTITY" {
		t.Fatalf("err = %v, want INVALID_QUANTITY", err)
	}
}
