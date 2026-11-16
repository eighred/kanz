package replay

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/factormodel"
	factorpb "github.com/eighred/kanz/kanz-schemas-go/factor/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fittedSource struct {
	model *factormodel.Model
	calls int
}

func (s *fittedSource) Model(context.Context, time.Time) (*factormodel.Model, bool) {
	s.calls++
	return s.model, s.model != nil
}

func TestFactorRiskReconstructsWithoutRefitting(t *testing.T) {
	p := testPortfolio()
	ts := timestamppb.New(p.AsOf())
	m, err := factormodel.FromSnapshot(&factorpb.FactorModelSnapshot{
		Model:           &factorpb.FactorModel{ModelId: "fitted-v1", AsOf: ts, Factors: []*factorpb.Factor{{Name: "market", Type: factorpb.FactorType_FACTOR_TYPE_STATISTICAL}}},
		Exposures:       []*factorpb.FactorExposure{{ModelId: "fitted-v1", AsOf: ts, InstrumentId: "A", Loadings: []float64{.12345678901234567}, SpecificVariance: .00003}},
		Covariance:      &factorpb.FactorCovariance{ModelId: "fitted-v1", AsOf: ts, Dimension: 1, Values: []float64{.00002}},
		InputProvenance: map[string]string{"input_digest": "dated-panel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	source := &fittedSource{model: m}
	ctx := context.Background()
	r := compute.DefaultRegistry()
	compute.RegisterFactorRisk(ctx, r, compute.FactorProviders{Model: Models{Source: source}})
	sink := new(recordSink)
	e, err := New(r, sink, "test")
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Compute(ctx, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	measure, _ := result.Measures.Lookup(compute.MeasureFactorVaR99)
	if measure.Value.GetCoefficient() == 0 || measure.Provenance.InputDigest == "" {
		t.Fatal("factor fixture did not produce attributable risk")
	}
	source.model = nil
	if _, err := reconstruct(ctx, sink.record); err != nil {
		t.Fatal(err)
	}
	if source.calls != 1 {
		t.Fatalf("factor refit or duplicate lookup: %d", source.calls)
	}
}
