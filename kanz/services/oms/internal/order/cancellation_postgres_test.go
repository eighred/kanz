package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/internal/platform/halt"
	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func waitForOrderStoreLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, application string, done <-chan error) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock')`, application).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("writer did not wait for transaction: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func requestedParent() *orderpb.OrderState {
	return &orderpb.OrderState{OrderId: "parent", PortfolioId: "fund-alpha",
		Status: orderpb.OrderStatus_ORDER_STATUS_WORKING_SCHEDULED,
		CancellationRequest: &orderpb.OrderCancellationRequest{
			Command: &orderpb.CancelOrder{OrderId: "parent", Metadata: &commandpb.CommandMetadata{
				Issuer: "operator", PrincipalPortfolios: []string{"fund-alpha"}}},
			RequestedAt: timestamppb.New(t0), CorrelationId: "withdrawal", CausationId: "cancel-command",
		}}
}

func TestPostgresCancellationBarrierRejectsLateChildAndDispatch(t *testing.T) {
	pool := newPool(t)
	ctx := testCtx()
	store := NewPostgres(pool)
	parent := requestedParent()
	request := parent.CancellationRequest
	parent.CancellationRequest = nil
	if err := store.Create(ctx, parent, nil); err != nil {
		t.Fatal(err)
	}
	child := &orderpb.OrderState{OrderId: "child", ParentOrderId: "parent", PortfolioId: "fund-alpha", Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	if err := store.Create(ctx, child, nil); err != nil {
		t.Fatal(err)
	}
	parent.CancellationRequest = request
	if err := store.Save(ctx, parent, 0, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, child, nil); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate became a rejected child: %v", err)
	}
	late := proto.Clone(child).(*orderpb.OrderState)
	late.OrderId = "late"
	if err := store.Create(ctx, late, nil); !errors.Is(err, ErrParentStopped) {
		t.Fatalf("late admission: %v", err)
	}
	child.Status = orderpb.OrderStatus_ORDER_STATUS_ROUTED
	fact, err := NewEmitter(&fakeBus{}).RoutedFact(ctx, child.OrderId, "XTEST", "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, child, 0, []outbox.Record{fact}, ""); !errors.Is(err, ErrParentStopped) {
		t.Fatalf("late dispatch: %v", err)
	}
	got, version, err := store.Load(ctx, child.OrderId)
	if err != nil || version != 0 || got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW {
		t.Fatalf("refused dispatch changed child: %v %d %v", got, version, err)
	}
	var records int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&records); err != nil || records != 0 {
		t.Fatalf("refused dispatch announced: %d %v", records, err)
	}
	parent.CancellationRequest = nil
	if err := store.Save(ctx, parent, 1, nil, ""); !errors.Is(err, ErrCancellationPending) {
		t.Fatalf("erased cancellation: %v", err)
	}
	parent.CancellationRequest = request
	parent.Status = orderpb.OrderStatus_ORDER_STATUS_EXPIRED
	if err := store.Save(ctx, parent, 1, nil, ""); !errors.Is(err, ErrCancellationPending) {
		t.Fatalf("retired cancelled schedule: %v", err)
	}
}

func TestPostgresChildWaitsForParentCancellationCommit(t *testing.T) {
	pool := newPool(t)
	ctx, cancel := context.WithTimeout(testCtx(), 10*time.Second)
	defer cancel()
	store := NewPostgres(pool)
	parent := requestedParent()
	request := parent.CancellationRequest
	parent.CancellationRequest = nil
	if err := store.Create(ctx, parent, nil); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM orders WHERE order_id='parent' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["application_name"] = "cancellation-barrier-test"
	replicaPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer replicaPool.Close()
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		done <- NewPostgres(replicaPool).Create(ctx, &orderpb.OrderState{OrderId: "late", ParentOrderId: "parent", PortfolioId: "fund-alpha", Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}, nil)
	}()
	defer func() {
		cancel()
		_ = tx.Rollback(context.Background())
		<-joined
	}()
	waitForOrderStoreLock(t, ctx, pool, "cancellation-barrier-test", done)
	parent.CancellationRequest = request
	blob, err := proto.Marshal(parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE orders SET state=$1,version=version+1 WHERE order_id='parent'`, blob); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrParentStopped) {
		t.Fatalf("child crossed committed cancellation: %v", err)
	}
	if _, _, err := store.Load(ctx, "late"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late child exists: %v", err)
	}
}

func TestPostgresSaveKeepsExecutionClaimBeforeOrderLock(t *testing.T) {
	pool := newPool(t)
	ctx, cancel := context.WithTimeout(testCtx(), 10*time.Second)
	defer cancel()
	store := NewPostgres(pool)
	if err := store.Create(ctx, state("execution", orderpb.OrderStatus_ORDER_STATUS_ROUTED), nil); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// Recovery takes the execution claim before the order CAS. A live fold
	// must not hold the order while waiting for this claim, or both deadlock.
	if claimed, err := store.claimFill(ctx, tx, "execution", "fill", "fill"); err != nil || !claimed {
		t.Fatalf("recovery claim: %v %v", claimed, err)
	}
	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["application_name"] = "execution-lock-order-test"
	replicaPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer replicaPool.Close()
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		done <- NewPostgres(replicaPool).Save(ctx, state("execution", orderpb.OrderStatus_ORDER_STATUS_FILLED), 0, claimFacts("execution", "fill"), "fill")
	}()
	defer func() { cancel(); _ = tx.Rollback(context.Background()); <-joined }()
	waitForOrderStoreLock(t, ctx, pool, "execution-lock-order-test", done)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM orders WHERE order_id='execution' FOR UPDATE NOWAIT`); err != nil {
		t.Fatalf("live writer reversed recovery lock order: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrFillApplied) {
		t.Fatalf("duplicate execution: %v", err)
	}
}

func TestPostgresScheduleCancellationResumesAfterRestart(t *testing.T) {
	pool := newPool(t)
	ctx := bus.WithCausationID(bus.WithCorrelationID(testCtx(), "withdrawal"), "cancel-command")
	venue := &closerVenue{mic: "XTEST", err: errors.New("withdrawal timeout")}
	makeService := func(p *pgxpool.Pool, fb *fakeBus) *Service {
		t.Helper()
		s, err := NewService(testTenant, NewPostgres(p), NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{venue}), execution.NewCloseRegistry(), nil, WithHaltGate(halt.OpenGate(nil)))
		if err != nil {
			t.Fatal(err)
		}
		s.now = func() time.Time { return schedStart }
		return s
	}
	first := makeService(pool, &fakeBus{})
	if err := first.Handle(ctx, submitEnv(), mustMarshal(t, scheduledOrder("parent", 6, nil))); err != nil {
		t.Fatal(err)
	}
	if n, err := first.DriveSchedules(ctx); err != nil || n != 1 {
		t.Fatalf("first slice: %d %v", n, err)
	}
	command := &orderpb.CancelOrder{OrderId: "parent", Metadata: &commandpb.CommandMetadata{Issuer: "operator", PrincipalPortfolios: []string{"fund-alpha"}}}
	if err := first.Handle(ctx, cancelEnv(), mustMarshal(t, command)); err == nil {
		t.Fatal("unconfirmed child cancellation succeeded")
	}
	parent, _, err := first.store.Load(ctx, "parent")
	if err != nil || parent.GetCancellationRequest() == nil || IsTerminal(parent) {
		t.Fatalf("missing durable pending intent: %v %v", parent, err)
	}
	restartedPool, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer restartedPool.Close()
	venue.err = nil
	restarted := makeService(restartedPool, &fakeBus{})
	restarted.now = func() time.Time { return schedEnd.Add(time.Hour) }
	if n, err := restarted.DriveSchedules(testCtx()); err != nil || n != 0 {
		t.Fatalf("restart emitted new children: %d %v", n, err)
	}
	parent, _, err = restarted.store.Load(ctx, "parent")
	if err != nil || parent.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED || !proto.Equal(parent.GetCancellationRequest().GetCommand(), command) {
		t.Fatalf("restart lost withdrawal: %v %v", parent, err)
	}
	children, err := restarted.store.ListByParent(ctx, "parent")
	if err != nil || len(children) != 1 || !IsTerminal(children[0]) {
		t.Fatalf("children after restart: %v %v", children, err)
	}
	var correlation, causation string
	if err := pool.QueryRow(ctx, `SELECT correlation_id,causation_id FROM outbox WHERE partition_key='parent' AND event_type=$1`, EventTypeCancelled).Scan(&correlation, &causation); err != nil {
		t.Fatal(err)
	}
	if correlation != "withdrawal" || causation != "cancel-command" {
		t.Fatalf("lost cancellation lineage: %q %q", correlation, causation)
	}
}
