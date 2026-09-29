package varmodel

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/store"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/factormodel"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDatedRiskPanelThroughRealPostgres(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var super, bypass bool
	if err := pool.QueryRow(ctx, "SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("non-superuser/non-BYPASSRLS role required: %v", err)
	}
	schema := pgx.Identifier{fmt.Sprintf("dated_risk_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := pool.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migration, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "services", "market-data", "migrations", "0001_price_history.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	s := store.NewPostgres(db)
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	observation := func(id string, d int, cents int64, known int) store.Observation {
		return store.Observation{InstrumentID: id, ObservationTime: day(d), KnowledgeTime: day(known), Kind: store.PriceKindClose, CurrencyCode: "USD", Price: &commonpb.Decimal{Coefficient: cents, Exponent: -2}}
	}
	var obs []store.Observation
	for i, cents := range []int64{10000, 9000, 18000, 9000, 9900, 9900, 10890} {
		obs = append(obs, observation("A", i+1, cents, i+1))
	}
	for _, x := range [][2]int64{{1, 10000}, {2, 12000}, {4, 6000}, {5, 9000}, {6, 8100}, {7, 8100}} {
		obs = append(obs, observation("B", int(x[0]), x[1], int(x[0])))
	}
	if err := s.Put(ctx, obs); err != nil {
		t.Fatal(err)
	}
	rp := returns.NewStoreReturnsProvider(s, returns.ReturnsConfig{Method: returns.ReturnSimple})
	panel, err := returns.Load(ctx, rp, []string{"B", "A"}, day(8), 250)
	if err != nil {
		t.Fatal(err)
	}
	if len(panel.Intervals) != 4 || panel.Contiguous() || panel.Digest == "" {
		t.Fatalf("unexpected panel: %+v", panel)
	}
	want := [][]float64{{-.1, .1, 0, .1}, {.2, .5, -.1, 0}}
	for i, row := range want {
		for j, v := range row {
			if math.Abs(panel.Values[i][j]-v) > 1e-12 {
				t.Fatalf("dated cell [%d,%d]=%v want %v", i, j, panel.Values[i][j], v)
			}
		}
	}
	p := domain.NewPortfolio(v1.PortfolioID("DATED"), domain.CurrencyCode("USD"))
	p.SetAggregate(domain.AggregateUpdate{AsOf: day(8), BaseCurrency: "USD"})
	for id, v := range map[string]int64{"A": 1000, "B": -500} {
		p.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(id), MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: v}}})
	}
	h := Historical(Config{})(ctx, p, rp)
	es := ExpectedShortfall(Config{})(ctx, p, rp)
	// Independent reference: dated P&Ls [-200,-150,50,100], R7 1% quantile.
	if dec.Float64Or(h.Value, 0) != 198.5 || dec.Float64Or(es.Value, 0) != 200 || h.Coverage.ExcludedCount != 0 {
		t.Fatalf("VaR=%v ES=%v coverage=%+v", h.Value, es.Value, h.Coverage)
	}
	if h.Provenance.InputDigest != panel.Digest {
		t.Fatal("measure lost panel identity")
	}
	m, err := factormodel.Fit(ctx, factormodel.Config{Type: factormodel.Statistical, StatFactors: 2}, []string{"B", "A"}, day(8), factormodel.Providers{Returns: rp})
	if err != nil {
		t.Fatal(err)
	}
	risk := m.Risk(map[string]float64{"A": 1000, "B": -500})
	if math.Abs(risk.Total*risk.Total-65000.0/3) > 1e-7 || m.InputProvenance["panel_digest"] != panel.Digest {
		t.Fatalf("factor covariance disagrees with dated reference: %+v", risk)
	}
	mc := MonteCarlo(Config{Draws: 100000, Seed: 17})(ctx, p, rp)
	if mc.Provenance.InputDigest != panel.Digest || math.Abs(dec.Float64Or(mc.Value, 0)-(50+2.326347874*math.Sqrt(65000.0/3))) > 4 {
		t.Fatalf("MC disagrees with independent normal reference: %+v", mc)
	}
	dd := MaxDrawdownAmount(Config{})(ctx, p, rp)
	if dd.Coverage.ExcludedCount == 0 || dec.Float64Or(dd.Value, -1) != 0 {
		t.Fatal("gapped history became a realized drawdown path")
	}
	// A later correction must not leak backward, even after rebuilding providers.
	if err := s.Put(ctx, []store.Observation{observation("A", 2, 18000, 10)}); err != nil {
		t.Fatal(err)
	}
	reopened := returns.NewStoreReturnsProvider(store.NewPostgres(db), returns.ReturnsConfig{Method: returns.ReturnSimple})
	before, err := returns.Load(ctx, reopened, []string{"A", "B"}, day(8), 250)
	if err != nil || before.Digest != panel.Digest {
		t.Fatalf("future correction leaked: %v", err)
	}
	after, err := returns.Load(ctx, reopened, []string{"A", "B"}, day(11), 250)
	if err != nil || after.Digest == panel.Digest || math.Abs(after.Values[0][0]-.8) > 1e-12 {
		t.Fatalf("correction revision missing: %+v %v", after, err)
	}
	// Unequal listing dates restrict the common interval set, never tail-shift it.
	if err := s.Put(ctx, []store.Observation{observation("NEW", 5, 1000, 5), observation("NEW", 6, 1100, 6), observation("NEW", 7, 1200, 7)}); err != nil {
		t.Fatal(err)
	}
	listed, err := returns.Load(ctx, rp, []string{"A", "B", "NEW"}, day(8), 250)
	if err != nil || len(listed.Intervals) != 2 || !listed.Intervals[0].Start.Equal(day(5)) {
		t.Fatalf("listing alignment: %+v %v", listed, err)
	}
	// Suspended/disjoint closes do not manufacture multi-day daily shocks.
	if err := s.Put(ctx, []store.Observation{observation("SUSPENDED", 1, 100, 1), observation("SUSPENDED", 3, 200, 3), observation("SUSPENDED", 5, 300, 5)}); err != nil {
		t.Fatal(err)
	}
	if _, err := factormodel.Fit(ctx, factormodel.Config{Type: factormodel.Statistical}, []string{"A", "SUSPENDED"}, day(8), factormodel.Providers{Returns: rp}); err == nil {
		t.Fatal("missing history zero-filled into factor model")
	}
}
