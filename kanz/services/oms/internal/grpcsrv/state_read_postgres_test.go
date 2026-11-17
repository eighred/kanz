package grpcsrv_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/services/oms/internal/grpcsrv"
	orderstore "github.com/eighred/kanz/services/oms/internal/order"
)

// Exercise the actual tenant-scoped Postgres.Load with the owned order/read
// schema. This does not submit orders or prove venue/command acceptance.
func TestStateReadPostgresPreservesTenantIsolation(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL is required for real recorded-state RLS proof")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bootstrap, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bootstrap.Close)
	var super, bypass bool
	if err := bootstrap.QueryRow(ctx, "SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user").Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatal("recorded-state RLS proof requires NOSUPERUSER/NOBYPASSRLS")
	}
	schema := fmt.Sprintf("state_read_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := bootstrap.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := bootstrap.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	poolFor := func(tenant string) *pgxpool.Pool {
		t.Helper()
		cfg, err := pgxpool.ParseConfig(url)
		if err != nil {
			t.Fatal(err)
		}
		if err := pg.Service.Configure(cfg); err != nil {
			t.Fatal(err)
		}
		cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			if _, err := conn.Exec(ctx, "SET search_path TO "+quoted); err != nil {
				return err
			}
			_, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant)
			return err
		}
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	a, b := poolFor("t1"), poolFor("t2")
	for _, name := range []string{"0001_orders.sql", "0005_orders_version.sql"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Exec(ctx, string(data)); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(pool *pgxpool.Pool, tenant, id string, coefficient int64) {
		t.Helper()
		row := &orderpb.OrderState{OrderId: id, PortfolioId: "P", InstrumentId: "I", Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED,
			OrderedQuantity: &commonpb.Decimal{Coefficient: coefficient, Exponent: -11}, AsOf: timestamppb.New(time.Now().UTC())}
		data, err := proto.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "INSERT INTO orders (tenant_id, order_id, status, state) VALUES ($1,$2,$3,$4)", tenant, id, int32(row.GetStatus()), data); err != nil {
			t.Fatal(err)
		}
	}
	seed(a, "t1", "O", 9007199254740993)
	seed(b, "t2", "O", -9007199254740991)
	seed(b, "t2", "foreign", 123)
	read := grpcsrv.NewStateRead(ctx, orderstore.NewPostgres(a), "t1", nil)
	resp, err := read.OrderStateRead(ctx, &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "O"})
	if err != nil || resp.GetOrderedQuantity().GetValue().GetCoefficient() != 9007199254740993 {
		t.Fatalf("tenant-scoped exact read: %v %v", resp, err)
	}
	_, hidden := read.OrderStateRead(ctx, &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "foreign"})
	_, absent := read.OrderStateRead(ctx, &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "absent"})
	if status.Code(hidden) != codes.NotFound || status.Code(absent) != codes.NotFound || status.Convert(hidden).Message() != status.Convert(absent).Message() {
		t.Fatalf("foreign row discovery: %v / %v", hidden, absent)
	}
	t.Run("server-deadline-bounds-shared-pool-acquisition", func(t *testing.T) {
		if a.Config().MaxConns != 4 {
			t.Fatal("fixture is not using the unchanged canonical four-connection service profile")
		}
		held := make([]*pgxpool.Conn, 0, 4)
		release := func() {
			for _, conn := range held {
				conn.Release()
			}
			held = nil
		}
		defer release()
		for range 4 {
			conn, err := a.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, conn)
		}
		read := grpcsrv.NewStateRead(ctx, orderstore.NewPostgres(a), "t1", nil)
		defer func() {
			read.CancelReads()
			release()
			joinCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := read.JoinReads(joinCtx); err != nil {
				t.Error(err)
			}
		}()
		done := make(chan error, 1)
		go func() {
			_, err := read.OrderStateRead(context.Background(), &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "O"})
			done <- err
		}()
		select {
		case err := <-done:
			if status.Code(err) != codes.DeadlineExceeded {
				t.Fatalf("pool acquisition escaped the server deadline: %v", err)
			}
		case <-time.After(6 * time.Second):
			t.Fatal("read waited indefinitely for an execution-store connection")
		}
		if a.Stat().AcquiredConns() != 4 {
			t.Fatal("timed-out read changed ownership of the fixture's execution connections")
		}
		release()
		fresh, err := read.OrderStateRead(ctx, &querypb.OrderStateReadRequest{PortfolioId: "P", OrderId: "O"})
		if err != nil || fresh.GetOrderedQuantity().GetValue().GetCoefficient() != 9007199254740993 {
			t.Fatalf("pool did not recover after bounded read refusal: %v %v", fresh, err)
		}
	})
}
