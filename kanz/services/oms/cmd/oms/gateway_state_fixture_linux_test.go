//go:build linux && state_read_integration

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/migrate"
	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/pkg/transport"
	"github.com/eighred/kanz/services/oms/internal/config"
	"github.com/eighred/kanz/services/oms/internal/order"
)

// The gateway integration starts this test binary to reach the OMS-owned read
// listener. Only this component imports the private store and seeds its fixture.
func TestServeRecordedStateForGatewayRead(t *testing.T) {
	if os.Getenv("TEST_OMS_READ_CHILD") != "1" {
		t.Skip("started by the isolated gateway state-read integration")
	}
	root, stop := signal.NotifyContext(t.Context(), syscall.SIGTERM)
	defer stop()
	db := os.Getenv("TEST_POSTGRES_URL")
	if db != "postgres://kanzapp@127.0.0.1:55432/kanzapp?sslmode=disable" {
		t.Fatal("expected the disposable loopback PostgreSQL fixture")
	}
	pool, err := pg.NewTenantPool(root, db, "tenant_a")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var restricted bool
	if err := pool.QueryRow(root, "SELECT NOT rolsuper AND NOT rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&restricted); err != nil || !restricted {
		t.Fatal("the fixture must use a restricted role")
	}
	prepareRecordedStateSchema(t, root, db)
	const policy = `SELECT c.relrowsecurity AND c.relforcerowsecurity AND row_security_active(c.oid)
		AND app_current_tenant() = $1
		AND (SELECT count(*) FROM pg_policy WHERE polrelid = c.oid) = 1
		AND EXISTS (SELECT 1 FROM pg_policy AS p WHERE p.polrelid = c.oid AND p.polname = 'tenant_isolation'
			AND pg_get_expr(p.polqual, p.polrelid) = '(tenant_id = app_current_tenant())'
			AND pg_get_expr(p.polwithcheck, p.polrelid) = '(tenant_id = app_current_tenant())')
		FROM pg_class AS c WHERE c.oid = 'orders'::regclass`
	var active bool
	if err := pool.QueryRow(root, policy, "tenant_a").Scan(&active); err != nil || !active {
		t.Fatal("the tenant-scoped orders policy must be active on the OMS connection")
	}
	row := &orderpb.OrderState{
		OrderId: "recorded-order", PortfolioId: "PF_A", InstrumentId: "FIXTURE",
		Status:          orderpb.OrderStatus_ORDER_STATUS_ROUTED,
		OrderedQuantity: &commonpb.Decimal{Coefficient: 9007199254740995, Exponent: -8},
		AsOf:            timestamppb.New(time.Now().UTC().Add(-time.Second)),
	}
	data, err := proto.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(root, "INSERT INTO orders (order_id, portfolio_id, status, state) VALUES ($1,$2,$3,$4)", row.GetOrderId(), row.GetPortfolioId(), int32(row.GetStatus()), data); err != nil {
		t.Fatal(err)
	}
	mesh, err := transport.NewMesh(root, os.Getenv("TEST_OMS_READ_SOCKET"))
	if err != nil {
		t.Fatal(err)
	}
	defer mesh.Close()
	shutdown, err := serveStateRead(root, config.Config{StateQueryListen: os.Getenv("TEST_OMS_READ_LISTEN"), Tenant: "tenant_a"}, mesh, order.NewPostgres(pool), readSurfaceLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := shutdown(); err != nil {
			t.Error(err)
		}
	}()
	ready := os.NewFile(3, "read-listener-ready")
	if ready == nil {
		t.Fatal("missing readiness pipe")
	}
	_, err = fmt.Fprintln(ready, "ready")
	ready.Close()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-root.Done():
	case <-time.After(45 * time.Second):
		t.Fatal("parent did not release the recorded state service")
	}
}

func prepareRecordedStateSchema(t *testing.T, root context.Context, db string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(root, 12*time.Second)
	defer cancel()
	migrations, err := migrate.Load(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pg.NewMigrationPool(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	applied, err := migrate.New(pool).Up(ctx, migrations)
	if err != nil || len(applied) != len(migrations) {
		t.Fatalf("fresh OMS schema requires every owned migration: applied %d of %d, error %v", len(applied), len(migrations), err)
	}
}
