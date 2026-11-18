package engine_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/marketdata/terms"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/termsource"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
)

func TestPostgresTermsProviderResolvesExactOptionDecimals(t *testing.T) {
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
	const instrumentID = "XOKX:FINE-C"
	const underlyingID = "BTC-USDT"
	listed := time.UnixMilli(1730448000000)
	// The canonical stored decimals independently encode the catalogue strike
	// 60000.000000001 and 3e-9 underlying units times a 0.2 multiplier.
	rec := terms.Record{InstrumentID: instrumentID, UnderlyingID: underlyingID, AsOf: listed, Kind: terms.KindOption,
		Terms: &referencepb.ContractTerms{InstrumentId: instrumentID, AsOf: timestamppb.New(listed),
			Terms: &referencepb.ContractTerms_Option{Option: &referencepb.OptionTerms{
				UnderlyingId: underlyingID, Expiry: timestamppb.New(time.UnixMilli(1735286400000)),
				OptionType: referencepb.OptionType_OPTION_TYPE_CALL, ExerciseStyle: referencepb.ExerciseStyle_EXERCISE_STYLE_EUROPEAN,
				Strike: &commonpb.Decimal{Coefficient: 60000000000001, Exponent: -9}, ContractMultiplier: &commonpb.Decimal{Coefficient: 6, Exponent: -10},
			}}}}
	store := terms.NewPostgres(pool)
	if err := store.Put(t.Context(), []terms.Record{rec}); err != nil {
		t.Fatal(err)
	}
	spec, state, err := termsource.NewProvider(store).OptionTerms(t.Context(), rec.InstrumentID, listed)
	if err != nil || state != compute.TermsResolved || spec.Strike != 60000.000000001 || spec.Multiplier != 6e-10 {
		t.Fatalf("resolved terms: %+v, %v, %v", spec, state, err)
	}
}
