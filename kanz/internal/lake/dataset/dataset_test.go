package dataset_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/lake/dataset"
	"github.com/eighred/kanz/internal/marketdata/store"
)

var day0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func day(i int) time.Time { return day0.AddDate(0, 0, i) }

func put(t *testing.T, s store.Store, inst string, obs, knowledge int, cents int64) {
	t.Helper()
	err := s.Put(context.Background(), []store.Observation{{
		InstrumentID:    inst,
		ObservationTime: day(obs),
		Price:           &commonpb.Decimal{Coefficient: cents, Exponent: -2},
		Kind:            store.PriceKindClose,
		KnowledgeTime:   day(knowledge),
	}})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
}

// seed lays a 30-day close series, each price known the day it applies.
func seed(t *testing.T, s store.Store, inst string) {
	for i := 1; i <= 30; i++ {
		put(t, s, inst, i, i, int64(10000+i*100)) // $100.00 + $i
	}
}

func TestMaterializeProducesPointInTimeFeatures(t *testing.T) {
	s := store.NewMemory()
	seed(t, s, "AAPL")
	m := dataset.NewMaterializer(s, nil, dataset.Config{})

	row, err := m.Materialize(context.Background(), dataset.Sample{InstrumentID: "AAPL", AsOf: day(30)})
	if err != nil {
		t.Fatal(err)
	}
	if !row.Complete {
		t.Errorf("row incomplete, want complete: features=%v", row.Features)
	}
	for _, k := range []string{"spot", "ret_last", "ret_mean", "vol"} {
		if _, ok := row.Features[k]; !ok {
			t.Errorf("missing feature %q (have %v)", k, row.Features)
		}
	}
	// spot as of day 30 is day-30's close: $130.00.
	if got := row.Features["spot"]; got != 130.0 {
		t.Errorf("spot = %v, want 130.0", got)
	}
}

// A late correction must be invisible before its KnowledgeTime (no future
// leakage) and visible after — the bitemporal read is correct both ways.
func TestMaterializeNoFutureLeakage(t *testing.T) {
	ctx := context.Background()
	s := store.NewMemory()
	seed(t, s, "AAPL")
	m := dataset.NewMaterializer(s, nil, dataset.Config{})

	before15, _ := m.Materialize(ctx, dataset.Sample{InstrumentID: "AAPL", AsOf: day(15)})
	before30, _ := m.Materialize(ctx, dataset.Sample{InstrumentID: "AAPL", AsOf: day(30)})

	// Restate day-10's close drastically, learned on day 25.
	put(t, s, "AAPL", 10, 25, 50000) // $500, knowledge day 25

	after15, err := m.Materialize(ctx, dataset.Sample{InstrumentID: "AAPL", AsOf: day(15)})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before15.Features, after15.Features) {
		t.Errorf("future leakage: a correction known on day 25 changed the day-15 row\n before=%v\n after =%v",
			before15.Features, after15.Features)
	}

	after30, err := m.Materialize(ctx, dataset.Sample{InstrumentID: "AAPL", AsOf: day(30)})
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(before30.Features, after30.Features) {
		t.Error("correction known on day 25 should change the day-30 row, but it is unchanged")
	}
}

type fakeFeatures struct{ asOf time.Time }

func (f *fakeFeatures) FeaturesAsOf(_ context.Context, _ string, asOf time.Time) (map[string]float64, error) {
	f.asOf = asOf
	return map[string]float64{"sentiment": 0.5}, nil
}

func TestMaterializeJoinsFeatureSourceAtSameHorizon(t *testing.T) {
	s := store.NewMemory()
	seed(t, s, "AAPL")
	fs := &fakeFeatures{}
	m := dataset.NewMaterializer(s, fs, dataset.Config{})

	row, err := m.Materialize(context.Background(), dataset.Sample{InstrumentID: "AAPL", AsOf: day(20)})
	if err != nil {
		t.Fatal(err)
	}
	if row.Features["feat_sentiment"] != 0.5 {
		t.Errorf("feature-source join missing: %v", row.Features)
	}
	// The join must be queried at the SAME knowledge horizon as the market reads.
	if !fs.asOf.Equal(day(20)) {
		t.Errorf("feature source queried at %v, want as-of day 20", fs.asOf)
	}
}
