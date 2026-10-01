package artifacts

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/internal/risk/publish"
	factorpb "github.com/eighred/kanz/kanz-schemas-go/factor/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func testStores(t *testing.T) (*Store, *Store) {
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
	schema := fmt.Sprintf("risk_artifacts_test_%d", time.Now().UnixNano())
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
	return New(a), New(b)
}

func modelFixture(at time.Time) *factorpb.FactorModelSnapshot {
	ts := timestamppb.New(at)
	return &factorpb.FactorModelSnapshot{
		Model:           &factorpb.FactorModel{ModelId: "model-v1", AsOf: ts, Factors: []*factorpb.Factor{{Name: "market", Type: factorpb.FactorType_FACTOR_TYPE_STATISTICAL}}},
		Exposures:       []*factorpb.FactorExposure{{ModelId: "model-v1", AsOf: ts, InstrumentId: "equity", Loadings: []float64{.12345678901234567}, SpecificVariance: .00003}},
		Covariance:      &factorpb.FactorCovariance{ModelId: "model-v1", AsOf: ts, Dimension: 1, Values: []float64{.00002}},
		InputProvenance: map[string]string{"input_digest": "retained-panel"},
	}
}

func TestDurableModelIdentityIsolationAndRestart(t *testing.T) {
	a, b := testStores(t)
	ctx := context.Background()
	if err := a.Check(ctx); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-48 * time.Hour).UTC()
	original := modelFixture(at)
	before := time.Now().Add(-time.Second)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if err := a.SaveModel(ctx, original); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if _, err := a.ModelAt(ctx, "model-v1", at, before); !errors.Is(err, ErrMissing) {
		t.Fatalf("future knowledge leaked: %v", err)
	}
	if _, err := b.ModelAt(ctx, "model-v1", at, retainedKnowledge(t, a)); !errors.Is(err, ErrMissing) {
		t.Fatalf("cross tenant read: %v", err)
	}
	if _, err := a.ModelAt(ctx, "model-v1", at.Add(-time.Nanosecond), retainedKnowledge(t, a)); !errors.Is(err, ErrMissing) {
		t.Fatalf("future effective version leaked: %v", err)
	}
	// Recreate the repository object: no in-process cache survives this read.
	restored, err := New(a.pool).ModelAt(ctx, "model-v1", at, retainedKnowledge(t, a))
	if err != nil {
		t.Fatal(err)
	}
	want, err := factormodel.FromSnapshot(original)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{"equity": 987654321}
	if got := restored.VaR(values, .99); got != want.VaR(values, .99) {
		t.Fatalf("replayed VaR=%v", got)
	}
	conflict := proto.Clone(original).(*factorpb.FactorModelSnapshot)
	conflict.Exposures[0].SpecificVariance *= 2
	if err := a.SaveModel(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting identity accepted: %v", err)
	}
	if _, err := a.pool.Exec(ctx, `UPDATE risk_model_artifacts SET payload=payload`); err == nil {
		t.Fatal("SQL UPDATE changed immutable artifact")
	}
	// The other tenant can retain its own distinct content under the same key.
	if err := b.SaveModel(ctx, conflict); err != nil {
		t.Fatal(err)
	}
}

func TestStartupRefusesMissingIsolation(t *testing.T) {
	a, _ := testStores(t)
	ctx := context.Background()
	if _, err := a.pool.Exec(ctx, `ALTER TABLE risk_model_artifacts NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx); err == nil {
		t.Fatal("startup accepted owner-bypassable history")
	}
}

func TestDurableCurveSurvivesHorizonWithoutRounding(t *testing.T) {
	a, _ := testStores(t)
	ctx := context.Background()
	at := time.Now().Add(-365 * 24 * time.Hour).UTC()
	c, err := curve.NewZeroCurve([]float64{.25, 1, 30}, []float64{.012345678901234567, .03234567890123456, .04234567890123456}, curve.Continuous, curve.LogLinearDF)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RecordCurve(ctx, "USD", at, c); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CurveAt(ctx, "USD", at, retainedKnowledge(t, a).Add(-time.Microsecond)); !errors.Is(err, ErrMissing) {
		t.Fatalf("pre-admission curve leaked: %v", err)
	}
	got, err := New(a.pool).CurveAt(ctx, "USD", at, retainedKnowledge(t, a))
	if err != nil {
		t.Fatal(err)
	}
	for _, tenor := range []float64{.25, .5, 1, 5, 30} {
		if math.Float64bits(got.Discount(tenor)) != math.Float64bits(c.Discount(tenor)) {
			t.Fatalf("DF(%v) changed", tenor)
		}
	}
	bad := publish.ToProtoCalibratedCurve("USD", at, c)
	bad.Curve.Points[0].Rate = nil
	if err := a.SaveCurve(ctx, bad); err == nil {
		t.Fatal("absent rate accepted as zero")
	}
}

// Admission is stamped by PostgreSQL. A client wall clock may lag that clock
// (observed 722 microseconds on Windows), even after the write has returned.
// Use the retained knowledge boundary instead of weakening the cutoff or sleeping.
func retainedKnowledge(t *testing.T, s *Store) time.Time {
	t.Helper()
	var at time.Time
	if err := s.pool.QueryRow(context.Background(), `SELECT max(recorded_at) FROM risk_model_artifacts`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}
