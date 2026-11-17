package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	risk "github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/eighred/kanz/internal/risk/state"
	factorpb "github.com/eighred/kanz/kanz-schemas-go/factor/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type decompositionProvider struct {
	model *factormodel.Model
	seen  time.Time
	calls int
}

func (p *decompositionProvider) Model(_ context.Context, at time.Time) (*factormodel.Model, bool) {
	p.seen, p.calls = at, p.calls+1
	return p.model, p.model != nil
}

func TestDecomposeRiskUsesFrozenBookAndSuppliedModel(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second)
	ts := timestamppb.New(at)
	m, err := factormodel.FromSnapshot(&factorpb.FactorModelSnapshot{
		Model:           &factorpb.FactorModel{ModelId: "snapshot-fixture", AsOf: ts, Factors: []*factorpb.Factor{{Name: "MKT", Type: factorpb.FactorType_FACTOR_TYPE_STATISTICAL}}},
		Covariance:      &factorpb.FactorCovariance{ModelId: "snapshot-fixture", AsOf: ts, Dimension: 1, Values: []float64{.04}},
		Exposures:       []*factorpb.FactorExposure{{ModelId: "snapshot-fixture", AsOf: ts, InstrumentId: "A", Loadings: []float64{1}, SpecificVariance: .01}, {ModelId: "snapshot-fixture", AsOf: ts, InstrumentId: "B", Loadings: []float64{1}, SpecificVariance: .01}},
		InputProvenance: map[string]string{"input_digest": "owned-fixture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &decompositionProvider{model: m}
	s := state.NewStore()
	e := engine.New(s, compute.DefaultRegistry(), risk.NewCache(), risk.NewDetector(), engine.WithFactorModel(provider))
	applyPosition(t, s, "PF", "A", 100, at)
	applyPosition(t, s, "PF", "B", -50, at)
	got, err := e.DecomposeRisk(context.Background(), v1.DecomposeRiskRequest{PortfolioID: "PF"})
	if err != nil || decValue(got.Decomposition.Total) != 15 || got.CurrencyCode != "USD" || !got.AsOf.Equal(at) || !provider.seen.Equal(at) || provider.calls != 1 || got.Decomposition.Provenance.ModelID != "snapshot-fixture" {
		t.Fatalf("real engine/model binding: %+v %v provider=%+v", got, err, provider)
	}
	// A new unknown holding changes coverage, not the original returned snapshot.
	applyPosition(t, s, "PF", "UNKNOWN", 500, at.Add(time.Second))
	partial, err := e.DecomposeRisk(context.Background(), v1.DecomposeRiskRequest{PortfolioID: "PF"})
	if err != nil || !hasFlag(partial.QualityFlags, v1.QualityFlagInputsUnresolved) || partial.Decomposition.Coverage.ExcludedCount != 1 || len(got.Decomposition.ByInstrument) != 2 || got.Decomposition.Coverage.ExcludedCount != 0 {
		t.Fatalf("coverage/frozen response: %+v %v", partial, err)
	}
}

func TestDecomposeRiskRefusesBeforeProviderAndNeverUsesCachedLiveForHistory(t *testing.T) {
	s := state.NewStore()
	provider := &decompositionProvider{}
	e := engine.New(s, nil, risk.NewCache(), risk.NewDetector(), engine.WithFactorModel(provider), engine.WithOwnership(fixedOwnership{mine: "MINE", owner: "other"}))
	for _, tc := range []struct {
		request v1.DecomposeRiskRequest
		want    error
	}{
		{v1.DecomposeRiskRequest{}, v1.ErrInvalidRequest},
		{v1.DecomposeRiskRequest{PortfolioID: "FOREIGN"}, v1.ErrPortfolioNotOwned},
		{v1.DecomposeRiskRequest{PortfolioID: "MINE"}, v1.ErrPortfolioNotFound},
		{v1.DecomposeRiskRequest{PortfolioID: "MINE", AsOf: time.Now()}, v1.ErrInvalidRequest},
	} {
		if _, err := e.DecomposeRisk(context.Background(), tc.request); !errors.Is(err, tc.want) {
			t.Fatalf("%+v: %v want %v", tc.request, err, tc.want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.DecomposeRisk(ctx, v1.DecomposeRiskRequest{PortfolioID: "MINE"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if provider.calls != 0 {
		t.Fatal("refused request reached provider")
	}
}
