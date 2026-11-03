package order

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/services/oms/internal/capital"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type capitalScanTraceKey struct{}

// The trace pauses only after the real child scan finishes. It creates the
// previously untested interleaving without replacing PostgreSQL or its locks.
type capitalScanBarrier struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *capitalScanBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == "SELECT order_id,state FROM orders WHERE parent_order_id=$1" {
		return context.WithValue(ctx, capitalScanTraceKey{}, true)
	}
	return ctx
}

func (b *capitalScanBarrier) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(capitalScanTraceKey{}) != true {
		return
	}
	b.once.Do(func() {
		close(b.reached)
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	})
}

func capitalLockPool(t *testing.T, ctx context.Context, source *pgxpool.Pool, application string, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	cfg := source.Config().Copy()
	cfg.ConnConfig.RuntimeParams["application_name"] = application
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedCapitalLockBalances(t *testing.T, ctx context.Context, pool *pgxpool.Pool, now time.Time) []*commonpb.Money {
	t.Helper()
	for _, cash := range []struct{ currency, total string }{{"BNB", "3"}, {"USD", "100"}} {
		if err := capital.Apply(ctx, pool, capital.CashEvent{PortfolioID: "fund", Currency: cash.currency, Revision: 1,
			Total: dec.Exact(cash.total), Complete: true, ObservedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	return []*commonpb.Money{{CurrencyCode: "BNB", Amount: d(2, 0)}, {CurrencyCode: "USD", Amount: d(100, 0)}}
}

func TestPostgresCapitalCancellationLocksCashBeforeScanningChildren(t *testing.T) {
	pool := newPool(t)
	ctx, cancel := context.WithTimeout(testCtx(), 15*time.Second)
	defer cancel()
	now := time.Now().UTC()
	debits := seedCapitalLockBalances(t, ctx, pool, now)
	parent := &orderpb.OrderState{OrderId: "parent", PortfolioId: "fund", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, Status: orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED,
		OrderedQuantity: d(1, 0), LimitPrice: d(100, 0), ExecutionSchedule: &orderpb.ExecutionSchedule{}}
	if err := NewPostgres(pool).CreateFunded(ctx, parent, nil, debits, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	barrier := &capitalScanBarrier{reached: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	defer release()
	cancelPool := capitalLockPool(t, ctx, pool, "capital-cancel-scan", barrier)
	childPool := capitalLockPool(t, ctx, pool, "capital-child-after-scan", nil)
	terminal := proto.Clone(parent).(*orderpb.OrderState)
	terminal.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- NewPostgres(cancelPool).SaveFundedTerminal(ctx, terminal, 0, nil, now) }()
	select {
	case <-barrier.reached:
	case err := <-cancelDone:
		t.Fatalf("cancellation never reached the child scan: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	child := &orderpb.OrderState{OrderId: "late-child", ParentOrderId: "parent", PortfolioId: "fund", InstrumentId: "BTC-USD",
		Side: orderpb.Side_SIDE_BUY, OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW,
		OrderedQuantity: d(1, 0), LimitPrice: d(100, 0)}
	childDone := make(chan error, 1)
	go func() { childDone <- NewPostgres(childPool).CreateFunded(ctx, child, nil, debits, now, time.Minute) }()
	// Before the repair, the child commits here while cancellation is paused.
	// Observe an actual database lock wait, not a guessed scheduling delay.
	waitForOrderStoreLock(t, ctx, pool, "capital-child-after-scan", childDone)
	release()
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
	if err := <-childDone; !errors.Is(err, ErrParentStopped) {
		t.Fatalf("child crossed committed cancellation: %v", err)
	}
	var children int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE parent_order_id='parent'`).Scan(&children); err != nil || children != 0 {
		t.Fatalf("surviving children=%d err=%v", children, err)
	}
	for _, currency := range []string{"BNB", "USD"} {
		var reserved string
		if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency=$1`, currency).Scan(&reserved); err != nil || reserved != "0" {
			t.Fatalf("%s reserved=%s err=%v", currency, reserved, err)
		}
	}
}

func TestPostgresCapitalTerminalFillLocksUnusedFeeCurrencyFirst(t *testing.T) {
	pool := newPool(t)
	ctx, cancel := context.WithTimeout(testCtx(), 15*time.Second)
	defer cancel()
	now := time.Now().UTC()
	debits := seedCapitalLockBalances(t, ctx, pool, now)
	store := NewPostgres(pool)
	st := &orderpb.OrderState{OrderId: "fee-unused", PortfolioId: "fund", InstrumentId: "BTC-USD", Side: orderpb.Side_SIDE_BUY,
		OrderType: orderpb.OrderType_ORDER_TYPE_LIMIT, OrderedQuantity: d(1, 0), LeavesQuantity: d(1, 0), LimitPrice: d(100, 0),
		Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	if err := store.CreateFunded(ctx, st, nil, debits, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	fill := &orderpb.Fill{OrderId: st.OrderId, FillId: "no-fee-fill", InstrumentId: st.InstrumentId, Side: st.Side,
		Quantity: d(1, 0), Price: d(90, 0), ExecutedAt: timestamppb.New(now)}
	current, version, err := store.Load(ctx, st.OrderId)
	if err != nil {
		t.Fatal(err)
	}
	next, err := ApplyFill(current, fill, now)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := NewEmitter(&fakeBus{}).FillFact(ctx, fill, next)
	if err != nil {
		t.Fatal(err)
	}
	held, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Rollback(context.Background()) }()
	if _, err := held.Exec(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='BNB' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	fillPool := capitalLockPool(t, ctx, pool, "capital-terminal-no-fee", nil)
	done := make(chan error, 1)
	go func() { done <- NewPostgres(fillPool).Save(ctx, next, version, []outbox.Record{fact}, fill.FillId) }()
	waitForOrderStoreLock(t, ctx, pool, "capital-terminal-no-fee", done)
	// While BNB blocks the first lock, USD must still be free. The old path
	// held USD and then waited for unused BNB when releasing the terminal order.
	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Rollback(context.Background()) }()
	if _, err := probe.Exec(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency='USD' FOR UPDATE NOWAIT`); err != nil {
		t.Fatalf("terminal fill locked USD before unused BNB: %v", err)
	}
	if err := probe.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := held.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, expected := range []struct{ currency, reserved string }{{"BNB", "0"}, {"USD", "90"}} {
		var reserved string
		if err := pool.QueryRow(ctx, `SELECT reserved FROM capital_balances WHERE portfolio_id='fund' AND currency=$1`, expected.currency).Scan(&reserved); err != nil || reserved != expected.reserved {
			t.Fatalf("%s reserved=%s err=%v", expected.currency, reserved, err)
		}
	}
}
