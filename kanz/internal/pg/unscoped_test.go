package pg_test

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// unscopedConn opens a connection the way a service that forgot to scope itself
// would: no AfterConnect, no GUC. It exists only so the test above can prove such a
// connection cannot read anything.
func unscopedConn(ctx context.Context, dsn string) (*pgx.Conn, error) {
	return pgx.Connect(ctx, dsn)
}
