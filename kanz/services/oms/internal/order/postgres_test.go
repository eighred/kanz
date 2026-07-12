package order

// Postgres order-store integration tests (EXEC-M7c). Gated on TEST_POSTGRES_URL
// — they skip without a database.
//
// The load-bearing test here is TestPostgresCreateIsAtomicAdmissionGate. Every
// other test in this file would also pass against the in-memory store; that one
// is the reason the durable store exists. It runs concurrent Creates of ONE
// order_id through the engine and asserts exactly one caller is told it won,
// because the caller that wins is the caller that routes to a live venue.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to run order Postgres integration tests")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", "__system__")
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	applySchema(t, pool)
	return pool
}

func applySchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS orders CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	files, err := filepath.Glob(filepath.Join("../../migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob migrations: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		ddl, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("apply migration %s: %v", f, err)
		}
	}
}

func state(id string, status orderpb.OrderStatus) *orderpb.OrderState {
	return &orderpb.OrderState{OrderId: id, Status: status}
}

// TestPostgresCreateIsAtomicAdmissionGate is the whole point of the durable
// store: N concurrent deliveries of one order_id, exactly one admission. Under
// the in-memory store this held only within a process; here it must hold at the
// engine, which is what makes a multi-replica OMS safe.
func TestPostgresCreateIsAtomicAdmissionGate(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	const racers = 32
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		admitted int
		rejected int
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together, so they collide inside the engine
			err := st.Create(ctx, state("ORD-RACE", orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				admitted++
			case errors.Is(err, ErrExists):
				rejected++
			default:
				t.Errorf("unexpected create error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	// Exactly one caller may believe it owns this order. Any other number is a
	// double-trade (>1) or a dropped order (0).
	if admitted != 1 {
		t.Fatalf("admitted %d callers for one order_id, want exactly 1 (a second admission is a second trade)", admitted)
	}
	if rejected != racers-1 {
		t.Fatalf("rejected %d, want %d", rejected, racers-1)
	}
}

func TestPostgresCreateThenSaveThenLoad(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	if _, err := st.Load(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("load missing: want ErrNotFound, got %v", err)
	}
	if err := st.Create(ctx, state("ORD-1", orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW)); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A second Create of the same id loses the admission race, and writes nothing.
	if err := st.Create(ctx, state("ORD-1", orderpb.OrderStatus_ORDER_STATUS_FILLED)); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create: want ErrExists, got %v", err)
	}
	got, err := st.Load(ctx, "ORD-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW {
		t.Fatalf("losing Create overwrote state: status=%v, want PENDING_NEW", got.GetStatus())
	}

	// Save is the post-admission upsert: the transition lands.
	if err := st.Save(ctx, state("ORD-1", orderpb.OrderStatus_ORDER_STATUS_ROUTED)); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got, err = st.Load(ctx, "ORD-1"); err != nil || got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_ROUTED {
		t.Fatalf("save not applied: status=%v err=%v", got.GetStatus(), err)
	}
}

func TestPostgresListReturnsAllOrders(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	for _, id := range []string{"ORD-B", "ORD-A"} {
		if err := st.Create(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW)); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	got, err := st.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0].GetOrderId() != "ORD-A" || got[1].GetOrderId() != "ORD-B" {
		t.Fatalf("list mismatch: %+v", got)
	}
}
