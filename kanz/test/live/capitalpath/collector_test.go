package main

import (
	"context"
	"errors"
	"testing"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/fillfact"
)

func TestFactCollectorProvesTypedLineageAndEveryExecution(t *testing.T) {
	c := newFactCollector(config{
		tenant: "TENANT", portfolio: "PF", instrument: "BTC-USDT",
		venue: "XOKX", account: "OKX-DEMO-1", quantity: "0.0002",
	}, "order-1")

	mustObserveFact(t, c, subjectAccepted, &orderpb.OrderAccepted{
		OrderId: "order-1", State: &orderpb.OrderState{OrderId: "order-1", PortfolioId: "PF"},
	})
	mustObserveFact(t, c, subjectRouted, &orderpb.OrderRouted{OrderId: "order-1", Venue: "XOKX", VenueOrderId: "venue-9"})
	mustObserveFact(t, c, fillfact.SubjectPartiallyFilled, fillEvent(false, "fill-1"))
	mustObserveFact(t, c, fillfact.SubjectFilled, fillEvent(true, "fill-2"))
	mustObserveAccountingFact(t, c, "evt-fill-1")
	mustObserveAccountingFact(t, c, "evt-fill-2")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	fills, err := c.await(ctx)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if len(fills) != 2 || fills[0].GetFillId() != "fill-1" || fills[1].GetFillId() != "fill-2" {
		t.Fatalf("fills = %#v, want fill-1 then fill-2", fills)
	}
}

func TestFactCollectorFailsClosedOnSemanticDuplicateOrWrongAccount(t *testing.T) {
	tests := []struct {
		name string
		feed func(*testing.T, *factCollector)
		want string
	}{
		{
			name: "duplicate acceptance",
			feed: func(t *testing.T, c *factCollector) {
				fact := &orderpb.OrderAccepted{OrderId: "order-1", State: &orderpb.OrderState{OrderId: "order-1", PortfolioId: "PF"}}
				mustObserveFact(t, c, subjectAccepted, fact)
				mustObserveFact(t, c, subjectAccepted, fact)
			},
			want: "duplicate ORDER_ACCEPTED",
		},
		{
			name: "duplicate venue execution",
			feed: func(t *testing.T, c *factCollector) {
				mustObserveFact(t, c, fillfact.SubjectPartiallyFilled, fillEvent(false, "fill-1"))
				mustObserveFact(t, c, fillfact.SubjectPartiallyFilled, fillEvent(false, "fill-1"))
			},
			want: "duplicate fill_id",
		},
		{
			name: "wrong collateral account",
			feed: func(t *testing.T, c *factCollector) {
				ev := fillEvent(true, "fill-1").(*orderpb.OrderFilled)
				ev.Fill.VenueAccountId = "OTHER"
				mustObserveFact(t, c, fillfact.SubjectFilled, ev)
			},
			want: "venue_account_id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFactCollector(config{tenant: "TENANT", portfolio: "PF", instrument: "BTC-USDT", venue: "XOKX", account: "OKX-DEMO-1"}, "order-1")
			tt.feed(t, c)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := c.await(ctx)
			if err == nil || !contains(err.Error(), tt.want) {
				t.Fatalf("await error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestFactCollectorDoesNotLetAnotherTenantOrOrderSatisfyProof(t *testing.T) {
	c := newFactCollector(config{tenant: "TENANT", portfolio: "PF", instrument: "BTC-USDT", venue: "XOKX", account: "OKX-DEMO-1"}, "order-1")
	raw, err := proto.Marshal(&orderpb.OrderAccepted{OrderId: "other", State: &orderpb.OrderState{OrderId: "other", PortfolioId: "PF"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.handle(subjectAccepted)(context.Background(), &envelopepb.Envelope{TenantId: "OTHER", PartitionKey: "other", EventType: subjectAccepted}, raw); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err = c.await(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("await error = %v, want deadline (foreign fact must be ignored)", err)
	}
}

func TestFactCollectorRefusesACompletedLineageWhoseExecutionsDoNotEqualIntent(t *testing.T) {
	cfg := config{tenant: "TENANT", portfolio: "PF", instrument: "BTC-USDT", venue: "XOKX", account: "OKX-DEMO-1", quantity: "0.0003"}
	c := newFactCollector(cfg, "order-1")
	mustObserveFact(t, c, subjectAccepted, &orderpb.OrderAccepted{OrderId: "order-1", State: &orderpb.OrderState{OrderId: "order-1", PortfolioId: "PF"}})
	mustObserveFact(t, c, subjectRouted, &orderpb.OrderRouted{OrderId: "order-1", Venue: "XOKX", VenueOrderId: "venue-9"})
	mustObserveFact(t, c, fillfact.SubjectPartiallyFilled, fillEvent(false, "fill-1"))
	mustObserveFact(t, c, fillfact.SubjectFilled, fillEvent(true, "fill-2"))
	mustObserveAccountingFact(t, c, "evt-fill-1")
	mustObserveAccountingFact(t, c, "evt-fill-2")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.await(ctx)
	if err == nil || !contains(err.Error(), "executed quantity") {
		t.Fatalf("await error=%v, want exact executed quantity refusal", err)
	}
}

func mustObserveFact(t *testing.T, c *factCollector, subject string, message proto.Message) {
	t.Helper()
	raw, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	eventID := "evt-" + subject
	if filled, ok := message.(*orderpb.OrderFilled); ok {
		eventID = "evt-" + filled.GetFill().GetFillId()
	}
	if filled, ok := message.(*orderpb.OrderPartiallyFilled); ok {
		eventID = "evt-" + filled.GetFill().GetFillId()
	}
	env := &envelopepb.Envelope{EventId: eventID, TenantId: "TENANT", PartitionKey: "order-1", EventType: subject}
	if err := c.handle(subject)(context.Background(), env, raw); err != nil {
		t.Fatalf("handle %s: %v", subject, err)
	}
}

func mustObserveAccountingFact(t *testing.T, c *factCollector, causationID string) {
	t.Helper()
	c.armLive()
	raw, err := proto.Marshal(&accountingpb.PortfolioCashBalance{PortfolioId: "PF"})
	if err != nil {
		t.Fatal(err)
	}
	env := &envelopepb.Envelope{
		EventId: "accounted-" + causationID, CausationId: causationID,
		TenantId: "TENANT", PartitionKey: "PF", EventType: subjectPortfolioCash,
	}
	if err := c.handle(subjectPortfolioCash)(context.Background(), env, raw); err != nil {
		t.Fatalf("handle accounting FACT: %v", err)
	}
}

func fillEvent(final bool, id string) proto.Message {
	fill := &orderpb.Fill{
		FillId: id, OrderId: "order-1", InstrumentId: "BTC-USDT", Side: orderpb.Side_SIDE_BUY,
		Quantity: &commonpb.Decimal{Coefficient: 1, Exponent: -4},
		Price:    &commonpb.Decimal{Coefficient: 50000}, Venue: "XOKX", VenueAccountId: "OKX-DEMO-1",
		ExecutedAt: timestamppb.New(time.Unix(1_700_000_000, 0)),
	}
	state := &orderpb.OrderState{
		OrderId: "order-1", PortfolioId: "PF", InstrumentId: "BTC-USDT", Side: orderpb.Side_SIDE_BUY,
		OrderedQuantity: &commonpb.Decimal{Coefficient: 2, Exponent: -4},
		FilledQuantity:  &commonpb.Decimal{Coefficient: 1, Exponent: -4},
	}
	if final {
		state.Status = orderpb.OrderStatus_ORDER_STATUS_FILLED
		state.FilledQuantity = &commonpb.Decimal{Coefficient: 2, Exponent: -4}
		return &orderpb.OrderFilled{OrderId: "order-1", Fill: fill, State: state}
	}
	return &orderpb.OrderPartiallyFilled{OrderId: "order-1", Fill: fill, State: state}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
