package order

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
)

func TestPostgresScheduledChildrenUseOneParentCommitment(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: "100", Complete: true, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	store := NewPostgres(pool)
	parent := &orderpb.OrderState{OrderId: "parent", PortfolioId: "fund", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, Status: orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED,
		OrderedQuantity: d(1, 0), LimitPrice: d(100, 0), ExecutionSchedule: &orderpb.ExecutionSchedule{}}
	if err := store.CreateFunded(ctx, parent, nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(100, 0)}}, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	child := func(id string, coeff int64) *orderpb.OrderState {
		return &orderpb.OrderState{OrderId: id, ParentOrderId: "parent", PortfolioId: "fund", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
			OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW,
			OrderedQuantity: d(coeff, -1), LimitPrice: d(100, 0)}
	}
	for _, st := range []*orderpb.OrderState{child("first", 6), child("second", 4)} {
		if err := store.CreateFunded(ctx, st, nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(60, 0)}}, now, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CreateFunded(ctx, child("over", 1), nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(10, 0)}}, now, time.Minute); !errors.Is(err, capital.ErrInvalid) {
		t.Fatalf("over-allocation: %v", err)
	}
	wrong := child("wrong-price", 1)
	wrong.LimitPrice = d(101, 0)
	if err := store.CreateFunded(ctx, wrong, nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(101, 0)}}, now, time.Minute); !errors.Is(err, capital.ErrInvalid) {
		t.Fatalf("price escalation: %v", err)
	}
	var owners, members int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capital_commitments`).Scan(&owners); err != nil || owners != 1 {
		t.Fatalf("owners=%d err=%v", owners, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM capital_members`).Scan(&members); err != nil || members != 3 {
		t.Fatalf("members=%d err=%v", members, err)
	}
	var allocated, reserved string
	if err := pool.QueryRow(ctx, `SELECT allocated_quantity::text FROM orders WHERE order_id='parent'`).Scan(&allocated); err != nil || allocated != "1.0" && allocated != "1" {
		t.Fatalf("allocated=%s err=%v", allocated, err)
	}
	if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD'`).Scan(&reserved); err != nil || reserved != "100" {
		t.Fatalf("double reservation=%s err=%v", reserved, err)
	}
	if err := store.SaveFundedTerminal(ctx, &orderpb.OrderState{OrderId: "parent", PortfolioId: "fund", Status: orderpb.OrderStatus_ORDER_STATUS_CANCELLED}, 0, nil, now); !errors.Is(err, ErrParentStopped) {
		t.Fatalf("parent released while children worked: %v", err)
	}
	for _, id := range []string{"first", "second"} {
		if err := store.SaveFundedTerminal(ctx, &orderpb.OrderState{OrderId: id, ParentOrderId: "parent", PortfolioId: "fund", Status: orderpb.OrderStatus_ORDER_STATUS_CANCELLED}, 0, nil, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveFundedTerminal(ctx, &orderpb.OrderState{OrderId: "parent", PortfolioId: "fund", Status: orderpb.OrderStatus_ORDER_STATUS_CANCELLED}, 0, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateFunded(ctx, child("late", 1), nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(10, 0)}}, now, time.Minute); !errors.Is(err, ErrParentStopped) {
		t.Fatalf("late child=%v", err)
	}
}

func TestPostgresScheduledChildCannotCrossParentCancellation(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: "USD", Revision: 1, Total: "100", Complete: true, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	parent := &orderpb.OrderState{OrderId: "parent", PortfolioId: "fund", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, Status: orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED,
		OrderedQuantity: d(1, 0), LimitPrice: d(100, 0), ExecutionSchedule: &orderpb.ExecutionSchedule{}}
	if err := NewPostgres(pool).CreateFunded(ctx, parent, nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(100, 0)}}, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		results <- NewPostgres(pool).CreateFunded(ctx, &orderpb.OrderState{OrderId: "child", ParentOrderId: "parent", PortfolioId: "fund",
			InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT,
			Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW, OrderedQuantity: d(1, 0), LimitPrice: d(100, 0)},
			nil, []*commonpb.Money{{CurrencyCode: "USD", Amount: d(100, 0)}}, now, time.Minute)
	})
	wg.Go(func() {
		<-start
		results <- NewPostgres(pool).SaveFundedTerminal(ctx, &orderpb.OrderState{OrderId: "parent", PortfolioId: "fund",
			Status: orderpb.OrderStatus_ORDER_STATUS_CANCELLED}, 0, nil, now)
	})
	close(start)
	wg.Wait()
	close(results)
	var succeeded, refused int
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrParentStopped) {
			refused++
		} else {
			t.Fatalf("race produced unexpected error: %v", err)
		}
	}
	if succeeded != 1 || refused != 1 {
		t.Fatalf("race outcomes: succeeded=%d refused=%d", succeeded, refused)
	}
	var parentStatus int32
	var children int
	if err := pool.QueryRow(ctx, `SELECT status FROM orders WHERE order_id='parent'`).Scan(&parentStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE parent_order_id='parent'`).Scan(&children); err != nil {
		t.Fatal(err)
	}
	cancelled := int32(orderpb.OrderStatus_ORDER_STATUS_CANCELLED)
	if parentStatus == cancelled && children != 0 || parentStatus != cancelled && children != 1 {
		t.Fatalf("parent status %d with %d children", parentStatus, children)
	}
}
