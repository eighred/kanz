package consume_test

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/services/accounting/internal/ledger"
	"github.com/eighred/kanz/services/accounting/internal/server"
)

// A separate read-only process serves the actual accounting history route.
// The parent owns its lifetime via stdin and reaches it through the gateway.
func TestSyntheticCashHistoryServer(t *testing.T) {
	dsn := os.Getenv("SYNTHETIC_BOOK_HISTORY_URL")
	if dsn == "" {
		t.Skip("requires a disposable cash-history reader")
	}
	const tenant = "synthetic-cash-clock"
	db, err := url.Parse(dsn)
	if err != nil || db.Scheme != "postgres" || db.Host != "127.0.0.1:5432" || db.User.Username() != "kanz_synthetic_cash_history" || db.Path != "/kanz_synthetic_accounting" || db.RawQuery != "sslmode=disable" {
		t.Fatal("expected the isolated read-only accounting login")
	}
	pool, err := pg.NewTenantPool(context.Background(), dsn, tenant)
	if err != nil {
		t.Fatal("open accounting history")
	}
	defer pool.Close()
	var restricted bool
	if err := pool.QueryRow(t.Context(), `SELECT NOT (rolsuper OR rolbypassrls) AND NOT has_table_privilege(current_user,'cash_commit_history','INSERT')
		FROM pg_roles WHERE rolname=current_user`).Scan(&restricted); err != nil || !restricted {
		t.Fatal("history reader exceeds its grant")
	}
	api := httptest.NewServer(server.New(&server.Readiness{}, nil, ledger.NewPostgres(pool), "USD", server.WithTenant(tenant)))
	defer api.Close()
	fmt.Fprintln(os.Stdout, api.URL)
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}
