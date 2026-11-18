package termsload

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/marketdata/terms"
)

func TestOKXExactOptionDecimalsPersistInPostgres(t *testing.T) {
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
	if len(records) != 1 {
		t.Fatalf("loaded %d contracts, want one", len(records))
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
	option := stored.Terms.GetOption()
	if option.GetStrike().GetCoefficient() != 60000000000001 || option.GetStrike().GetExponent() != -9 ||
		option.GetContractMultiplier().GetCoefficient() != 6 || option.GetContractMultiplier().GetExponent() != -10 {
		t.Fatalf("stored option lost precision: %v", option)
	}
	if _, err := store.LatestAsOf(t.Context(), rec.InstrumentID, listed.Add(-time.Microsecond)); !errors.Is(err, terms.ErrNoTerms) {
		t.Fatalf("prelisting read: %v", err)
	}
	correction := expiry.Add(time.Hour)
	corrected, err := ParseOKXInstruments(okxCatalogue(strings.Replace(preciseOptionRow(), "60000.000000001", "60001", 1)), mapping(), Options{AsOfOverride: correction})
	if err != nil {
		t.Fatal(err)
	}
	amended, err := corrected.Loadable()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Put(t.Context(), amended); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		at   time.Time
		want terms.Record
	}{{listed, rec}, {correction.Add(-time.Microsecond), rec}, {correction, amended[0]}} {
		chain, err := store.ChainAsOf(t.Context(), rec.UnderlyingID, tc.at, terms.KindOption)
		if err != nil || len(chain) != 1 || !proto.Equal(chain[0].Terms, tc.want.Terms) {
			t.Fatalf("chain at %s: %v, %v", tc.at, chain, err)
		}
	}
}
