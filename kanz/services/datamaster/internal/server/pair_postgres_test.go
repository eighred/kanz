package server

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/eighred/kanz/internal/refdata"
	"github.com/eighred/kanz/services/datamaster/internal/feed"
	"github.com/eighred/kanz/services/datamaster/internal/master"
	"github.com/eighred/kanz/services/datamaster/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Exercises the actual CSV decoder, survivorship, durable golden record,
// datamaster HTTP handler and OMS reference client. No network response stub.
func TestPostgresPairQuoteSurvivesGoldenRecordAndHTTP(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_URL required for durable pair reference verification")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "datamaster_pair_test"
	cfg.ConnConfig.RuntimeParams["app.tenant_id"] = testTenant
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var bypass bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&bypass); err != nil || bypass {
		t.Fatalf("test requires NOSUPERUSER NOBYPASSRLS: bypass=%v err=%v", bypass, err)
	}
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS datamaster_pair_test CASCADE; CREATE SCHEMA datamaster_pair_test`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DROP SCHEMA datamaster_pair_test CASCADE`); err != nil {
			t.Error(err)
		}
	})
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatal("migrations", err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	path := filepath.Join(t.TempDir(), "reference.csv")
	if err := os.WriteFile(path, []byte("symbol,asset_class,currency,base_asset,quote_asset,as_of\nPAIR,CRYPTO,USD,BTC,USDT,2026-10-03T00:00:00Z\nUNKNOWN,CRYPTO,USD,,,2026-10-03T00:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := feed.NewFileRefSource("vendor", 0, path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := feed.NewReferenceAdapter(source, nil).Records(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	golden := store.NewPostgresGolden(pool)
	for _, row := range rows {
		record, _ := master.Resolve([]master.VendorRecord{row})
		if err := golden.Put(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(New(nil, nil, testTenant, golden, nil, nil))
	t.Cleanup(srv.Close)
	client, err := refdata.NewHTTPSource(srv.URL, testTenant, "svc:oms")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"PAIR", "UNKNOWN"} {
		record, found, err := client.Fetch(ctx, id)
		if err != nil || !found {
			t.Fatalf("%s: found=%v err=%v", id, found, err)
		}
		if record.CurrencyCode != "USD" {
			t.Fatalf("lost denomination: %+v", record)
		}
		if id == "PAIR" && (record.BaseAsset != "BTC" || record.QuoteAsset != "USDT") {
			t.Fatalf("lost pair: %+v", record)
		}
		if id == "UNKNOWN" && (record.BaseAsset != "" || record.QuoteAsset != "") {
			t.Fatalf("guessed pair: %+v", record)
		}
	}
}
