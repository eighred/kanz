// Package syntheticblotter starts the existing OMS query service against a
// disposable tenant database. It has no command consumers or mutation routes.
package syntheticblotter

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/services/oms/internal/grpcsrv"
	"github.com/eighred/kanz/services/oms/internal/order"
	"google.golang.org/grpc"
)

// Start returns the loopback gRPC target and an owned shutdown function.
func Start(ctx context.Context, dsn, tenant string) (string, func(), error) {
	if dsn == "" || tenant == "" {
		return "", nil, errors.New("fixture database and tenant required")
	}
	pool, err := pg.NewTenantPool(ctx, dsn, tenant)
	if err != nil {
		return "", nil, errors.New("restricted OMS fixture database unavailable")
	}
	var restricted bool
	err = pool.QueryRow(ctx, `SELECT NOT (r.rolsuper OR r.rolbypassrls)
		AND c.relrowsecurity AND c.relforcerowsecurity
		AND current_setting('app.tenant_id') = $1
		FROM pg_roles r JOIN pg_class c ON c.oid = 'orders'::regclass
		WHERE r.rolname = current_user`, tenant).Scan(&restricted)
	if err != nil || !restricted {
		pool.Close()
		return "", nil, errors.New("tenant role and forced orders RLS required")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		pool.Close()
		return "", nil, err
	}
	service := grpc.NewServer()
	store := order.NewPostgres(pool)
	grpcsrv.New(store, store.Proposals(), time.Now, tenant).Register(service)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = service.Serve(listener)
	}()
	return listener.Addr().String(), func() {
		service.Stop()
		<-done
		pool.Close()
	}, nil
}
