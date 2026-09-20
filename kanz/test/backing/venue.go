// Package backing provisions isolated real-service fixtures for integration tests.
package backing

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
	"github.com/jackc/pgx/v5"
)

// VenueStore applies the deployed migrations in a throwaway schema. Each call
// to the returned factory opens a new tenant-scoped pool, as an adapter restart does.
func VenueStore(t testing.TB, migrations string) func() *orderview.Postgres {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires restricted-role TEST_POSTGRES_URL")
	}
	ctx := context.Background()
	boot, err := pg.NewMigrationPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(boot.Close)
	var bypass bool
	if err := boot.QueryRow(ctx, `SELECT COALESCE(bool_or(rolsuper OR rolbypassrls),false) FROM pg_roles WHERE pg_has_role(current_user,oid,'USAGE')`).Scan(&bypass); err != nil {
		t.Fatal(err)
	}
	if bypass {
		t.Fatal("fixture role bypasses RLS")
	}
	schema := fmt.Sprintf("pending_close_%d", time.Now().UnixNano())
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := boot.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	open := func() *orderview.Postgres {
		pool, err := pg.NewTenantPool(ctx, u.String(), "fund-alpha")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return orderview.NewPostgres(pool)
	}
	migrationPool, err := pg.NewTenantPool(ctx, u.String(), "fund-alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer migrationPool.Close()
	files, err := filepath.Glob(filepath.Join(migrations, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migration files missing: %v", err)
	}
	for _, file := range files {
		ddl, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := migrationPool.Exec(ctx, string(ddl)); err != nil {
			t.Fatal(err)
		}
	}
	return open
}
