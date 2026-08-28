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
	"google.golang.org/protobuf/proto"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
		if _, err := conn.Exec(ctx, "SET search_path TO "+testSchema); err != nil {
			return err
		}
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

// testSchema isolates this suite's tables in their OWN Postgres schema.
//
// `go test ./...` runs packages IN PARALLEL against ONE database, and migration 0002
// rewrites the RLS policy on EVERY table in current_schema(). Two suites replaying their
// migrations into `public` at the same time therefore fight over each other's tables
// ("tuple concurrently updated", "relation ... does not exist") — a race that grows with
// every table any service adds. A schema per suite ends it: nothing this service's
// migrations create is visible to anybody else's DO-block.
const testSchema = "oms_order_test"

func applySchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	// The pool's connections already point at the schema, so it must exist before they
	// are used — recreate it on a connection of our own.
	admin, err := pgxpool.New(ctx, os.Getenv("TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, `DROP SCHEMA IF EXISTS `+testSchema+` CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+testSchema); err != nil {
		t.Fatalf("create schema: %v", err)
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
			err := st.Create(ctx, state("ORD-RACE", orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW), nil)
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

	if _, _, err := st.Load(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("load missing: want ErrNotFound, got %v", err)
	}
	if err := st.Create(ctx, state("ORD-1", orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW), nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A second Create of the same id loses the admission race, and writes nothing.
	if err := st.Create(ctx, state("ORD-1", orderpb.OrderStatus_ORDER_STATUS_FILLED), nil); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create: want ErrExists, got %v", err)
	}
	got, ver, err := st.Load(ctx, "ORD-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW {
		t.Fatalf("losing Create overwrote state: status=%v, want PENDING_NEW", got.GetStatus())
	}

	// Save is the post-admission compare-and-swap: the transition lands when the
	// caller still holds the version it loaded.
	if err := st.Save(ctx, state("ORD-1", orderpb.OrderStatus_ORDER_STATUS_ROUTED), ver, nil, ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got, _, err = st.Load(ctx, "ORD-1"); err != nil || got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_ROUTED {
		t.Fatalf("save not applied: status=%v err=%v", got.GetStatus(), err)
	}
}

func TestPostgresListReturnsAllOrders(t *testing.T) {
	pool := newPool(t)
	st := NewPostgres(pool)
	ctx := context.Background()

	for _, id := range []string{"ORD-B", "ORD-A"} {
		if err := st.Create(ctx, state(id, orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW), nil); err != nil {
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

// ORDER HISTORY BY PORTFOLIO, AND THE PART OF IT THAT CANNOT BE SEEN (#399).
//
// The read itself is unremarkable. What has to hold is the honesty around it:
// an order admitted before migration 0007 carries an empty portfolio_id, because
// its real one is inside a protobuf blob no SQL statement can decode. Those
// orders can never appear in this result, so the caller is told how many there
// are — a partial history presented as a complete one is the failure this
// repository refuses everywhere else.
func TestPostgresListByPortfolioReportsWhatItCannotIndex(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	store := NewPostgres(pool)

	inPortfolio := func(id, portfolio string) *orderpb.OrderState {
		return &orderpb.OrderState{OrderId: id, PortfolioId: portfolio,
			Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	}
	for _, o := range []*orderpb.OrderState{
		inPortfolio("o1", "flagship"),
		inPortfolio("o2", "flagship"),
		inPortfolio("o3", "research"),
	} {
		if err := store.Create(ctx, o, nil); err != nil {
			t.Fatalf("create %s: %v", o.GetOrderId(), err)
		}
	}

	// A pre-0007 row, written the only way one can now exist: the column landed
	// with DEFAULT '' and the default was dropped, so this is what an order
	// admitted by the previous code looks like.
	if _, err := pool.Exec(ctx, `
		INSERT INTO orders (tenant_id, order_id, status, state, portfolio_id)
		VALUES (current_setting('app.tenant_id'), 'legacy-1', 1, $1, '')`,
		mustMarshalState(t, inPortfolio("legacy-1", "flagship"))); err != nil {
		t.Fatalf("seed a pre-0007 order: %v", err)
	}

	got, unindexed, err := store.ListByPortfolio(ctx, "flagship", 0)
	if err != nil {
		t.Fatalf("ListByPortfolio: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d orders, want 2 — the other portfolio's order and the unindexed one "+
			"must not appear", len(got))
	}
	for _, o := range got {
		if o.GetPortfolioId() != "flagship" {
			t.Errorf("order %s belongs to %q", o.GetOrderId(), o.GetPortfolioId())
		}
	}

	// THE ASSERTION THAT MATTERS. Without it the caller shows two orders and the
	// operator believes that is the portfolio's whole history.
	if unindexed != 1 {
		t.Fatalf("unindexed = %d, want 1.\n\n"+
			"An order that predates 0007 cannot be selected by portfolio at all, so the count "+
			"is the only way a caller can say 'these are the orders we can index' rather than "+
			"'these are the orders'.", unindexed)
	}

	// A SAVE BACKFILLS THE COLUMN. It is derived from the blob and every write
	// rewrites it, so an old order becomes indexable the moment anything touches
	// it — no separate backfill pass is required for a live order.
	st, ver, err := store.Load(ctx, "legacy-1")
	if err != nil {
		t.Fatalf("load legacy-1: %v", err)
	}
	if err := store.Save(ctx, st, ver, nil, ""); err != nil {
		t.Fatalf("save legacy-1: %v", err)
	}
	got, unindexed, err = store.ListByPortfolio(ctx, "flagship", 0)
	if err != nil {
		t.Fatalf("ListByPortfolio after save: %v", err)
	}
	if len(got) != 3 || unindexed != 0 {
		t.Fatalf("after re-saving the legacy order: %d orders / %d unindexed, want 3 / 0 — "+
			"every Save rewrites portfolio_id from the blob, so touching an order indexes it",
			len(got), unindexed)
	}
}

// THE EMPTY ID IS A MARKER, NOT A PORTFOLIO. Querying for it would return
// precisely the orders whose portfolio is unknown, presented as if they belonged
// to a portfolio named "".
func TestPostgresListByPortfolioRefusesTheEmptyID(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	store := NewPostgres(pool)

	if _, err := pool.Exec(ctx, `
		INSERT INTO orders (tenant_id, order_id, status, state, portfolio_id)
		VALUES (current_setting('app.tenant_id'), 'legacy-2', 1, $1, '')`,
		mustMarshalState(t, &orderpb.OrderState{OrderId: "legacy-2"})); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, _, err := store.ListByPortfolio(ctx, "", 0)
	if err != nil {
		t.Fatalf("ListByPortfolio(\"\"): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an empty portfolio id returned %d orders — that is the NOT-INDEXED marker, "+
			"and answering it hands back exactly the orders nobody can attribute", len(got))
	}
}

func mustMarshalState(t *testing.T, st *orderpb.OrderState) []byte {
	t.Helper()
	b, err := proto.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	return b
}
