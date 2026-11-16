package okx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

func TestOKXUserData_NegativeCumulativeIsRefusedAndQuarantined(t *testing.T) {
	for _, status := range []string{"partially_filled", "filled"} {
		t.Run(status, func(t *testing.T) {
			view := newOKXOrders()
			if err := view.store.Record(context.Background(), negativeQuantityOrder("negative1620")); err != nil {
				t.Fatal(err)
			}
			capture := &okxCapture{}
			refused := 0
			ing := newOKXUserDataIngester(OKXUserDataConfig{
				Stream: &okxStream{frames: [][]byte{[]byte(fmt.Sprintf(
					`{"arg":{"channel":"orders"},"data":[{"instId":"BTC-USDT","clOrdId":"negative1620","state":%q,"fillSz":"1","fillPx":"1","accFillSz":"-1","tradeId":"7","uTime":"1700000000000"}]}`, status))}},
				Orders: view, Pub: capture, Venue: "OKX", Tenant: "fund-alpha",
				OnRefused: func(mic, id, reason string) {
					refused++
					if mic != "OKX" || id != "negative1620" || reason != "unrepresentable" {
						t.Errorf("unexpected refusal identity/class: %s/%s/%s", mic, id, reason)
					}
				},
			})
			if err := ing.Run(context.Background()); !errors.Is(err, io.EOF) {
				t.Fatalf("report handling ended with %v, want the exhausted fixture stream", err)
			}
			if len(capture.events) != 0 || refused != 1 {
				t.Errorf("negative cumulative published %d events and counted %d refusals", len(capture.events), refused)
			}
			st := view.get(t, "negative1620")
			if st.GetQuarantine() == nil || st.GetQuarantine().GetReason() == "" {
				t.Error("the refused execution report did not quarantine the order with a reason")
			}
			if dec.FromProto(st.GetFilledQuantity()).Sign() != 0 || dec.FromProto(st.GetLeavesQuantity()).Cmp(big.NewRat(10, 1)) != 0 {
				t.Error("the refused cumulative changed the recorded fill or leaves")
			}
			if len(view.errs) != 0 {
				t.Errorf("order-view persistence reported %v", view.errs)
			}
		})
	}
}

func negativeQuantityOrder(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId: id, PortfolioId: "fund-alpha", InstrumentId: "BTC-USD",
		Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
		Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED, LimitPrice: &commonpb.Decimal{Coefficient: 1},
		OrderedQuantity: &commonpb.Decimal{Coefficient: 10}, FilledQuantity: &commonpb.Decimal{}, LeavesQuantity: &commonpb.Decimal{Coefficient: 10},
	}
}
