package factormodel_test

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/eighred/kanz/internal/risk/publish"
	factorpb "github.com/eighred/kanz/kanz-schemas-go/factor/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func retainedFixture() *factorpb.FactorModelSnapshot {
	at := timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s := &factorpb.FactorModelSnapshot{
		Model:      &factorpb.FactorModel{ModelId: "retained", AsOf: at, Factors: []*factorpb.Factor{{Name: "market", Type: factorpb.FactorType_FACTOR_TYPE_STATISTICAL}}},
		Covariance: &factorpb.FactorCovariance{ModelId: "retained", AsOf: at, Dimension: 1, Values: []float64{1}},
	}
	for _, id := range []string{"A", "B", "C"} {
		s.Exposures = append(s.Exposures, &factorpb.FactorExposure{ModelId: "retained", AsOf: at, InstrumentId: id, Loadings: []float64{1}})
	}
	return s
}

func TestRestoredRiskUsesStableSummationAndOwnsItsInputs(t *testing.T) {
	s := retainedFixture()
	m, err := factormodel.FromSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	s.Exposures[0].Loadings[0] = 999
	book := map[string]float64{"A": 1e16, "B": -1e16, "C": 1}
	for range 1000 {
		if got := m.Risk(book); got.Total != 1 || got.FactorExposure[0] != 1 {
			t.Fatalf("non-deterministic or aliased reconstruction: %+v", got)
		}
	}
	if row, ok := m.Loading("A"); !ok || row[0] != 1 {
		t.Fatal("reconstruction did not rebuild the instrument index")
	}
}

func TestRestoreRefusesCorruptModel(t *testing.T) {
	cases := map[string]func(*factorpb.FactorModelSnapshot){
		"missing identity":           func(s *factorpb.FactorModelSnapshot) { s.Model.ModelId = "" },
		"nil model":                  func(s *factorpb.FactorModelSnapshot) { s.Model = nil },
		"nil exposure":               func(s *factorpb.FactorModelSnapshot) { s.Exposures[0] = nil },
		"wrong instance":             func(s *factorpb.FactorModelSnapshot) { s.Exposures[0].ModelId = "other" },
		"duplicate instrument":       func(s *factorpb.FactorModelSnapshot) { s.Exposures[1].InstrumentId = "A" },
		"missing loading":            func(s *factorpb.FactorModelSnapshot) { s.Exposures[0].Loadings = nil },
		"nonfinite loading":          func(s *factorpb.FactorModelSnapshot) { s.Exposures[0].Loadings[0] = math.NaN() },
		"negative specific variance": func(s *factorpb.FactorModelSnapshot) { s.Exposures[0].SpecificVariance = -1 },
		"missing covariance":         func(s *factorpb.FactorModelSnapshot) { s.Covariance = nil },
		"short covariance":           func(s *factorpb.FactorModelSnapshot) { s.Covariance.Values = nil },
		"nonfinite covariance":       func(s *factorpb.FactorModelSnapshot) { s.Covariance.Values[0] = math.Inf(1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := proto.Clone(retainedFixture()).(*factorpb.FactorModelSnapshot)
			mutate(s)
			if _, err := factormodel.FromSnapshot(s); err == nil {
				t.Fatal("corrupt artifact accepted")
			}
		})
	}
}

func TestRestoreRejectsIndefiniteCovariance(t *testing.T) {
	s := retainedFixture()
	s.Model.Factors = append(s.Model.Factors, &factorpb.Factor{Name: "other", Type: factorpb.FactorType_FACTOR_TYPE_STATISTICAL})
	for _, e := range s.Exposures {
		e.Loadings = append(e.Loadings, 1)
	}
	s.Covariance.Dimension, s.Covariance.Values = 2, []float64{1, 2, 2, 1}
	if _, err := factormodel.FromSnapshot(s); err == nil {
		t.Fatal("negative variance direction accepted")
	}
}

func TestRestoreChecksCovarianceAcrossFactorUnits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values [][]float64
		valid  bool
	}{
		{"negative spread variance", [][]float64{{1e20, 0, 0}, {0, 1, 2}, {0, 2, 1}}, false},
		{"zero variance with a coupling", [][]float64{{1e20, 1e-100}, {1e-100, 0}}, false},
		// Every pairwise correlation is in [-1,1] but the (1,-1,-1)
		// contrast of the last three factors has negative variance.
		{"indefinite three-factor block", [][]float64{{1e20, 0, 0, 0}, {0, 1, .9, .9}, {0, .9, 1, -.9}, {0, .9, -.9, 1}}, false},
		// Outer products of (1e150, 1e-150, 0) give a singular PSD matrix.
		{"mixed-unit singular", [][]float64{{1e300, 1, 0}, {1, 1e-300, 0}, {0, 0, 0}}, true},
		{"subnormal independent", [][]float64{{math.MaxFloat64, 0}, {0, math.SmallestNonzeroFloat64}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := retainedFixture()
			s.Model.Factors = nil
			s.Covariance.Values = nil
			s.Covariance.Dimension = uint32(len(tc.values))
			for i, row := range tc.values {
				s.Model.Factors = append(s.Model.Factors, &factorpb.Factor{Name: fmt.Sprint("factor", i), Type: factorpb.FactorType_FACTOR_TYPE_STATISTICAL})
				s.Covariance.Values = append(s.Covariance.Values, row...)
			}
			for _, e := range s.Exposures {
				e.Loadings = make([]float64, len(tc.values))
				e.Loadings[0] = 1
			}
			m, err := factormodel.FromSnapshot(s)
			if !tc.valid {
				if err == nil || m != nil {
					t.Fatal("invalid covariance restored")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wire := publish.ToProtoFactorModelSnapshot(m)
			restored, err := factormodel.FromSnapshot(wire)
			if err != nil || !proto.Equal(s.Covariance, wire.Covariance) {
				t.Fatalf("covariance round trip: %v, %v", wire.Covariance, err)
			}
			got := restored.Risk(map[string]float64{"A": 1}).Total
			want := math.Sqrt(tc.values[0][0])
			if got != want {
				t.Fatalf("one-unit risk %g, want %g", got, want)
			}
		})
	}
}
