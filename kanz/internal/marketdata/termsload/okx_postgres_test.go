package termsload

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/marketdata/terms"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/termsource"
)

func TestOKXExactOptionReachesPostgresTermsProvider(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_URL for the OKX terms round trip")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("okx_terms_test_%d", time.Now().UnixNano())
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	name := pgx.Identifier{schema}.Sanitize()
	if _, err = pool.Exec(t.Context(), "CREATE SCHEMA "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DROP SCHEMA "+name+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	ddl, err := os.ReadFile(filepath.Join("..", "..", "..", "services", "market_data", "migrations", "0002_contract_terms.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(t.Context(), string(ddl)); err != nil {
		t.Fatal(err)
	}
	records, err := parse(t, okxCatalogue(preciseOptionRow())).Loadable()
	if err != nil {
		t.Fatal(err)
	}
	store := terms.NewPostgres(pool)
	if err = store.Put(t.Context(), records); err != nil {
		t.Fatal(err)
	}
	rec := records[0]
	stored, err := store.LatestAsOf(t.Context(), rec.InstrumentID, listed)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(stored.Terms, rec.Terms) {
		t.Fatalf("wire terms changed: %v", stored.Terms)
	}
	spec, state, err := termsource.NewProvider(store).OptionTerms(t.Context(), rec.InstrumentID, listed)
	if err != nil || state != compute.TermsResolved || spec.Strike != 60000.000000001 || spec.Multiplier != 6e-10 {
		t.Fatalf("resolved terms: %+v, %v, %v", spec, state, err)
	}
}
