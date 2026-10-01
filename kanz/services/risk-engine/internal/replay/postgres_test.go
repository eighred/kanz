package replay

import (
	"context"
	"errors"
	"fmt"
	risk "github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/state"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testRepositories(t *testing.T) (*Postgres, *Postgres) {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_URL required for real artifact persistence")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var privileged bool
	if err := admin.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&privileged); err != nil {
		t.Fatal(err)
	}
	if privileged {
		t.Fatal("artifact RLS proof requires NOSUPERUSER NOBYPASSRLS")
	}
	schema := fmt.Sprintf("risk_replay_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	makePool := func(tenant string) *pgxpool.Pool {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.RuntimeParams["search_path"] = schema
		cfg.ConnConfig.RuntimeParams["app.tenant_id"] = tenant
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	a, b := makePool("artifact-a"), makePool("artifact-b")
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Exec(ctx, string(body)); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	return NewPostgres(a), NewPostgres(b)
}

func TestPostgresReplayAtomicityIsolationAndRestart(t *testing.T) {
	a, b := testRepositories(t)
	ctx := context.Background()
	if err := a.Check(ctx); err != nil {
		t.Fatal(err)
	}
	sink := new(recordSink)
	evaluator, err := New(compute.DefaultRegistry(), sink, "test")
	if err != nil {
		t.Fatal(err)
	}
	p := testPortfolio()
	if _, err := evaluator.Compute(ctx, p, nil); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.Save(ctx, sink.record); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, err := a.Load(ctx, p.ID(), before); !errors.Is(err, ErrMissing) {
		t.Fatalf("knowledge cutoff: %v", err)
	}
	cutoff := evaluationKnowledge(t, a)
	if _, err := b.Load(ctx, p.ID(), cutoff); !errors.Is(err, ErrMissing) {
		t.Fatalf("tenant isolation: %v", err)
	}
	if _, err := a.Load(ctx, p.ID(), p.AsOf().Add(-time.Nanosecond)); !errors.Is(err, ErrMissing) {
		t.Fatalf("effective cutoff: %v", err)
	}
	restarted, err := New(compute.DefaultRegistry(), NewPostgres(a.pool), "different-build")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Replay(ctx, p.ID(), cutoff); err != nil {
		t.Fatal(err)
	}
	// A cold live store/cache cannot supply this answer. Historical reads must
	// reconstruct the retained evaluation without poisoning the current cache.
	cache := risk.NewCache()
	query := engine.New(state.NewStore(), compute.DefaultRegistry(), cache, risk.NewDetector(), engine.WithEvaluator(restarted))
	response, err := query.Measures(ctx, v1.MeasuresRequest{PortfolioID: p.ID(), AsOf: cutoff, Measures: []v1.MeasureName{compute.MeasureGrossExposure}})
	if err != nil {
		t.Fatalf("historical query: %v", err)
	}
	if _, ok := response.Set.Lookup(compute.MeasureGrossExposure); !ok {
		t.Fatal("requested measure missing")
	}
	if _, ok := response.Set.Lookup(compute.MeasureVaR99); ok {
		t.Fatal("measure filter ignored")
	}
	if _, ok := cache.LookupMeasures(p.ID()); ok {
		t.Fatal("historical answer poisoned current cache")
	}
	if _, err := query.Exposure(ctx, v1.ExposureRequest{PortfolioID: p.ID(), AsOf: cutoff}); err != nil {
		t.Fatal(err)
	}
	if _, err := query.Measures(ctx, v1.MeasuresRequest{PortfolioID: p.ID(), AsOf: before}); !errors.Is(err, v1.ErrHistoryUnavailable) {
		t.Fatalf("missing history: %v", err)
	}
	var count int
	if err := a.pool.QueryRow(ctx, `SELECT count(*) FROM risk_evaluations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate evaluations %d %v", count, err)
	}
	conflict := sink.record
	conflict.Measures = []byte("conflicting result")
	if err := a.Save(ctx, conflict); err == nil {
		t.Fatal("conflicting output accepted")
	}
	if _, err := restarted.Replay(ctx, p.ID(), cutoff); err != nil {
		t.Fatal("failed write damaged retained evaluation", err)
	}
	if _, err := a.pool.Exec(ctx, `UPDATE risk_evaluations SET measures=measures`); err == nil {
		t.Fatal("mutable evaluation")
	}
	if _, err := a.pool.Exec(ctx, `DELETE FROM risk_input_objects`); err == nil {
		t.Fatal("referenced input deleted")
	}
	if _, err := a.pool.Exec(ctx, `INSERT INTO risk_input_objects(digest,payload) VALUES($1,$2)`, string(make([]byte, 64)), []byte("wrong hash")); err == nil {
		t.Fatal("invalid object digest accepted")
	}
	if _, err := a.pool.Exec(ctx, `ALTER TABLE risk_evaluations NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx); err == nil {
		t.Fatal("unprotected store accepted")
	}
}

func evaluationKnowledge(t *testing.T, s *Postgres) time.Time {
	t.Helper()
	var at time.Time
	if err := s.pool.QueryRow(context.Background(), `SELECT max(recorded_at) FROM risk_evaluations`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func TestAsOfSelectsEarlierBookAndSharesUnchangedInputs(t *testing.T) {
	a, _ := testRepositories(t)
	ctx := context.Background()
	e, err := New(compute.DefaultRegistry(), a, "test")
	if err != nil {
		t.Fatal(err)
	}
	p := testPortfolio()
	p.SetPosition(domain.Position{InstrumentID: "B", MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 10}}})
	first, err := e.Compute(ctx, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := evaluationKnowledge(t, a)
	p.SetAggregate(domain.AggregateUpdate{AsOf: cutoff.Add(time.Nanosecond), BaseCurrency: "USD", PositionCount: 2})
	p.SetPosition(domain.Position{InstrumentID: "A", MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 45678, Exponent: -2}}})
	second, err := e.Compute(ctx, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	newCutoff := evaluationKnowledge(t, a)
	if newCutoff.Before(p.AsOf()) {
		newCutoff = p.AsOf()
	}
	restarted, err := New(compute.DefaultRegistry(), NewPostgres(a.pool), "restart")
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		at     time.Time
		digest string
	}{{cutoff, measureDigest(first.Measures)}, {newCutoff, measureDigest(second.Measures)}} {
		result, err := restarted.Replay(ctx, p.ID(), check.at)
		if err != nil {
			t.Fatal(err)
		}
		if measureDigest(result.Measures) != check.digest {
			t.Fatal("historical request selected the wrong book")
		}
	}
	var count int
	if err := a.pool.QueryRow(ctx, `SELECT count(*) FROM risk_input_objects`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("unchanged position was not deduplicated: %d %v", count, err)
	}
}

func measureDigest(ms *domain.MeasureSet) string {
	m, _ := ms.Lookup(compute.MeasureGrossExposure)
	return m.Provenance.InputManifestDigest
}
