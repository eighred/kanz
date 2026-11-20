package marketdatatest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/pg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool applies the market-data migrations in a private schema under a restricted role.
func NewPool(t *testing.T, ctx context.Context, dsn, migrations string) *pgxpool.Pool {
	t.Helper()
	boot, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = boot.Close(ctx) }()
	var privileged bool
	if err := boot.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE pg_has_role(current_user, oid, 'MEMBER') AND (rolsuper OR rolbypassrls))`).Scan(&privileged); err != nil {
		t.Fatal(err)
	}
	if privileged {
		t.Fatal("test requires a non-superuser/non-BYPASSRLS role, including memberships")
	}
	schema := fmt.Sprintf("market_boundary_%d", time.Now().UnixNano())
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := pgx.Connect(cleanupCtx, dsn)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close(cleanupCtx) }()
		if _, err := conn.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	// Preserve both DSN forms accepted by pgx. The schema name above uses
	// only an internal prefix and decimal digits.
	scopedDSN := dsn + " search_path=" + schema
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid test PostgreSQL URL")
		}
		params := u.Query()
		params.Set("search_path", schema)
		u.RawQuery = params.Encode()
		scopedDSN = u.String()
	}
	pool, err := pg.NewGlobalPool(ctx, scopedDSN, "synthetic global market observations in a disposable test schema")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, name := range []string{"0001_price_history.sql", "0003_ohlcv_bars.sql", "0004_ohlcv_trade_count_nullable.sql", "0005_ingestion_coverage.sql"} {
		data, err := os.ReadFile(filepath.Join(migrations, name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}
