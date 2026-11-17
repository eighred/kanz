package compute

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/factormodel"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	factorpb "github.com/eighred/kanz/kanz-schemas-go/factor/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var decompositionAt = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func decompositionModel(t *testing.T) *factormodel.Model {
	t.Helper()
	at := timestamppb.New(decompositionAt)
	m, err := factormodel.FromSnapshot(&factorpb.FactorModelSnapshot{
		Model:      &factorpb.FactorModel{ModelId: "owned-fixture", AsOf: at, Factors: []*factorpb.Factor{{Name: "MKT", Type: factorpb.FactorType_FACTOR_TYPE_STATISTICAL}}},
		Covariance: &factorpb.FactorCovariance{ModelId: "owned-fixture", AsOf: at, Dimension: 1, Values: []float64{.04}},
		Exposures: []*factorpb.FactorExposure{
			{ModelId: "owned-fixture", AsOf: at, InstrumentId: "A", Loadings: []float64{1}, SpecificVariance: .01},
			{ModelId: "owned-fixture", AsOf: at, InstrumentId: "B", Loadings: []float64{1}, SpecificVariance: .01},
		}, InputProvenance: map[string]string{"input_digest": "fixture-panel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func decompositionBook() *domain.Portfolio {
	p := domain.NewPortfolio("PF", "USD")
	// Reverse insertion proves ordering comes from keys, not insertion or maps.
	for id, value := range map[string]int64{"B": -50, "A": 100} {
		p.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(id), MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: value}}, AsOf: decompositionAt})
	}
	return p
}

func decompositionValue(t *testing.T, d *commonpb.Decimal) float64 {
	t.Helper()
	v, ok := dec.Float64(d)
	if !ok {
		t.Fatalf("absent/unrepresentable Decimal: %v", d)
	}
	return v
}

func TestDecomposePortfolioIndependentEulerAndProjection(t *testing.T) {
	d, err := DecomposePortfolio(context.Background(), decompositionBook(), staticModel{decompositionModel(t)})
	if err != nil {
		t.Fatal(err)
	}
	// Independently: Sigma=[[.05,.04],[.04,.05]], v=[100,-50].
	// Sigma*v=[3,1.5]; v^T*Sigma*v=225; Euler components=[20,-5].
	for _, check := range []struct {
		got  *commonpb.Decimal
		want float64
	}{
		{d.Total, 15}, {d.Systematic, 10}, {d.Specific, 11.180339887498949},
		{d.SpecificComponent, 25.0 / 3},
	} {
		if math.Abs(decompositionValue(t, check.got)-check.want) > 1e-12 {
			t.Fatalf("independent risk oracle: got %v want %.17g", check.got, check.want)
		}
	}
	if len(d.ByInstrument) != 2 || d.ByInstrument[0].InstrumentID != "A" || d.ByInstrument[1].InstrumentID != "B" || len(d.ByFactor) != 1 || d.ByFactor[0].Key != "MKT" {
		t.Fatalf("vector identity/order: %+v", d)
	}
	for i, want := range []float64{.2, .1} {
		if math.Abs(decompositionValue(t, d.ByInstrument[i].Marginal)-want) > 1e-15 {
			t.Fatalf("marginal %d: %v", i, d.ByInstrument[i])
		}
	}
	if decompositionValue(t, d.ByInstrument[0].Component) != 20 || decompositionValue(t, d.ByInstrument[1].Component) != -5 || math.Abs(decompositionValue(t, d.ByFactor[0].Component)-20.0/3) > 1e-12 {
		t.Fatal("signed component or factor contribution changed")
	}
	if d.Coverage.Contributed != 2 || d.Coverage.ExcludedCount != 0 || d.Provenance.ModelID != "owned-fixture" || !d.Provenance.ModelAsOf.Equal(decompositionAt) || d.Provenance.InputDigest != "fixture-panel" {
		t.Fatalf("coverage/provenance: %+v", d)
	}
	first := d.ByFactor[0].Component
	for range 50 {
		again, err := DecomposePortfolio(context.Background(), decompositionBook(), staticModel{decompositionModel(t)})
		if err != nil || !proto.Equal(first, again.ByFactor[0].Component) {
			t.Fatal("repeated decomposition changed")
		}
	}
}

func TestDecomposePortfolioExcludesUnknownInsteadOfZeroRows(t *testing.T) {
	p := decompositionBook()
	for _, pos := range []domain.Position{
		{InstrumentID: "C", MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 900}}},
		{InstrumentID: "D", MarketValue: &commonpb.Money{CurrencyCode: "EUR", Amount: &commonpb.Decimal{Coefficient: 900}}},
		{InstrumentID: "E", MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 1, Exponent: 65}}},
	} {
		p.SetPosition(pos)
	}
	d, err := DecomposePortfolio(context.Background(), p, staticModel{decompositionModel(t)})
	if err != nil || len(d.ByInstrument) != 2 || d.Coverage.Contributed != 2 || d.Coverage.ExcludedCount != 3 || len(d.Coverage.Exclusions) != 3 {
		t.Fatalf("coverage: %+v, %v", d, err)
	}
	for i, reason := range []string{SkipNotInModel, "currency_unresolved", "market_value_unresolved"} {
		if d.Coverage.Exclusions[i].Reason != reason {
			t.Fatalf("exclusion order/reason: %+v", d.Coverage)
		}
	}
	if decompositionValue(t, d.Total) != 15 {
		t.Fatal("excluded holdings entered arithmetic")
	}
}

func TestDecomposePortfolioRefusesUnknownFutureAndNonfiniteModels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*factormodel.Model)
	}{
		{"uncitable id", func(m *factormodel.Model) { m.ModelID = "" }},
		{"uncitable time", func(m *factormodel.Model) { m.AsOf = time.Time{} }},
		{"future", func(m *factormodel.Model) { m.AsOf = decompositionAt.Add(time.Second) }},
		{"missing input digest", func(m *factormodel.Model) { m.InputProvenance = nil }},
		{"nonfinite output", func(m *factormodel.Model) { m.FactorCov[0][0] = math.Inf(1) }},
		{"duplicate factor", func(m *factormodel.Model) { m.Factors = append(m.Factors, m.Factors[0]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := decompositionModel(t)
			tc.mutate(m)
			if d, err := DecomposePortfolio(context.Background(), decompositionBook(), staticModel{m}); !errors.Is(err, v1.ErrDecompositionUnavailable) || d.Total != nil {
				t.Fatalf("accepted unavailable model: %+v %v", d, err)
			}
		})
	}
	for _, provider := range []ModelProvider{nil, staticModel{nil}} {
		if _, err := DecomposePortfolio(context.Background(), decompositionBook(), provider); !errors.Is(err, v1.ErrDecompositionUnavailable) {
			t.Fatalf("missing model became success: %v", err)
		}
	}
}

func TestDecomposePortfolioKeepsSubEightDecimalObservation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		coefficient int64
		exponent    int32
	}{
		{"negative-exponent", 1, -9},
		{"positive-exponent", 1, 20},
		{"short-negative-exponent", -1, -9},
		{"short-positive-exponent", -1, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := domain.NewPortfolio("PF", "USD")
			input := &commonpb.Decimal{Coefficient: tc.coefficient, Exponent: tc.exponent}
			p.SetPosition(domain.Position{InstrumentID: "A", MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: input}, AsOf: decompositionAt})
			m := decompositionModel(t)
			m.FactorCov[0][0], m.SpecificVar["A"] = 1, 0
			d, err := DecomposePortfolio(context.Background(), p, staticModel{m})
			// One unit loading, unit variance and no specific risk give
			// sigma=|v| while factor exposure retains the position's sign.
			wantRisk := &commonpb.Decimal{Coefficient: 1, Exponent: tc.exponent}
			if err != nil || !proto.Equal(d.Total, wantRisk) || len(d.ByFactor) != 1 || !proto.Equal(d.ByFactor[0].Exposure, input) {
				t.Fatalf("lost shortest-roundtrip observation: %+v %v", d, err)
			}
		})
	}
}

func TestDecomposePortfolioBoundsAreCompleteAndCoverageSampleIsFinite(t *testing.T) {
	p := decompositionBook()
	for i := 2; i < v1.MaxDecompositionInstruments; i++ {
		p.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(fmt.Sprintf("z%04d", i)), AsOf: decompositionAt})
	}
	d, err := DecomposePortfolio(context.Background(), p, staticModel{decompositionModel(t)})
	if err != nil || d.Coverage.ExcludedCount != 1022 || len(d.Coverage.Exclusions) != v1.MaxInputExclusions {
		t.Fatalf("exact boundary/sample: %+v %v", d.Coverage, err)
	}
	p.SetPosition(domain.Position{InstrumentID: "too-many"})
	if _, err := DecomposePortfolio(context.Background(), p, staticModel{decompositionModel(t)}); !errors.Is(err, v1.ErrDecompositionTooLarge) {
		t.Fatalf("oversized book accepted: %v", err)
	}
	for _, key := range []string{strings.Repeat("x", 257), string([]byte{255})} {
		p = decompositionBook()
		p.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(key)})
		if _, err := DecomposePortfolio(context.Background(), p, staticModel{decompositionModel(t)}); !errors.Is(err, v1.ErrDecompositionTooLarge) {
			t.Fatalf("invalid/big key accepted: %v", err)
		}
	}
	m := decompositionModel(t)
	m.Factors = make([]factormodel.Factor, 65)
	if _, err := DecomposePortfolio(context.Background(), decompositionBook(), staticModel{m}); !errors.Is(err, v1.ErrDecompositionTooLarge) {
		t.Fatalf("oversized factors accepted: %v", err)
	}
}

func TestDecomposePortfolioExactFactorAndKeyBoundaries(t *testing.T) {
	m := decompositionModel(t)
	m.Factors = make([]factormodel.Factor, 64)
	m.FactorCov = make([][]float64, 64)
	for i := range m.Factors {
		m.Factors[i] = factormodel.Factor{Name: fmt.Sprintf("F%02d", i)}
		m.FactorCov[i] = make([]float64, 64)
	}
	m.FactorCov[0][0] = .04
	m.Loadings = [][]float64{make([]float64, 64), make([]float64, 64)}
	m.Loadings[0][0], m.Loadings[1][0] = 1, 1
	p := decompositionBook()
	p.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(strings.Repeat("x", 256))})
	d, err := DecomposePortfolio(context.Background(), p, staticModel{m})
	if err != nil || len(d.ByFactor) != 64 || len(d.ByInstrument) != 2 || d.Coverage.ExcludedCount != 1 {
		t.Fatalf("exact bounds refused: %+v %v", d, err)
	}
}

func TestDecomposePortfolioEmptyAndRisklessBooksAreCoveredZeros(t *testing.T) {
	p := domain.NewPortfolio("PF", "USD")
	p.SetSnapshot(decompositionAt, nil)
	d, err := DecomposePortfolio(context.Background(), p, staticModel{decompositionModel(t)})
	if err != nil || decompositionValue(t, d.Total) != 0 || len(d.ByInstrument) != 0 || len(d.ByFactor) != 1 || d.ByFactor[0].Key != "MKT" || decompositionValue(t, d.ByFactor[0].Exposure) != 0 || decompositionValue(t, d.ByFactor[0].Component) != 0 || d.Coverage.ExcludedCount != 0 {
		t.Fatalf("known empty book: %+v %v", d, err)
	}
	p = decompositionBook()
	m := decompositionModel(t)
	m.FactorCov[0][0], m.SpecificVar["A"], m.SpecificVar["B"] = 0, 0, 0
	d, err = DecomposePortfolio(context.Background(), p, staticModel{m})
	if err != nil || decompositionValue(t, d.Total) != 0 || len(d.ByInstrument) != 2 || len(d.ByFactor) != 1 || decompositionValue(t, d.ByFactor[0].Exposure) != 50 || decompositionValue(t, d.ByFactor[0].Component) != 0 || d.Coverage.Contributed != 2 {
		t.Fatalf("riskless covered book: %+v %v", d, err)
	}
}

func TestDecomposeModelRefusesUnrepresentableObservationWithoutRounding(t *testing.T) {
	m := decompositionModel(t)
	m.FactorCov[0][0], m.SpecificVar["A"] = 1, 0
	for _, value := range []float64{1e-61, 1e61, math.MaxFloat64} {
		if d, err := decomposeModel(m, map[string]float64{"A": value}); !errors.Is(err, v1.ErrDecompositionUnavailable) || d.Total != nil {
			t.Fatalf("unrepresentable float %.17g accepted: %+v %v", value, d, err)
		}
	}
}

func TestDecomposePortfolioHedgedZeroKeepsCompleteSortedFactorShape(t *testing.T) {
	m := decompositionModel(t)
	// Both instruments share two loadings. Their long/short hedge has zero
	// exposure to each known factor; factor identity must survive the zero.
	m.Factors = []factormodel.Factor{{Name: "Z"}, {Name: "A"}}
	m.Loadings = [][]float64{{1, 2}, {1, 2}}
	m.FactorCov = [][]float64{{.04, 0}, {0, .01}}
	m.SpecificVar["A"], m.SpecificVar["B"] = 0, 0
	p := decompositionBook()
	p.SetPosition(domain.Position{InstrumentID: "B", MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: -100}}, AsOf: decompositionAt})
	d, err := DecomposePortfolio(context.Background(), p, staticModel{m})
	if err != nil || decompositionValue(t, d.Total) != 0 || len(d.ByFactor) != 2 || d.ByFactor[0].Key != "A" || d.ByFactor[1].Key != "Z" || len(d.ByInstrument) != 2 || d.Coverage.Contributed != 2 {
		t.Fatalf("hedged full factor shape: %+v %v", d, err)
	}
	for _, f := range d.ByFactor {
		if decompositionValue(t, f.Exposure) != 0 || decompositionValue(t, f.Component) != 0 {
			t.Fatalf("hedged factor is not zero: %+v", f)
		}
	}
}

func TestDecomposePortfolioBoundsActualFitMetadataWithoutInventingIt(t *testing.T) {
	m := decompositionModel(t)
	for i := 0; i < 31; i++ {
		m.InputProvenance[fmt.Sprintf("p%02d", i)] = "recorded"
	}
	d, err := DecomposePortfolio(context.Background(), decompositionBook(), staticModel{m})
	if err != nil || len(d.Provenance.Params) != 32 || d.Provenance.Params["p00"] != "recorded" || d.Provenance.Params["confidence"] != "" {
		t.Fatalf("actual parameter projection: %+v %v", d.Provenance, err)
	}
	m.InputProvenance["extra"] = "recorded"
	if _, err := DecomposePortfolio(context.Background(), decompositionBook(), staticModel{m}); !errors.Is(err, v1.ErrDecompositionTooLarge) {
		t.Fatalf("oversized parameters accepted: %v", err)
	}
	m = decompositionModel(t)
	m.InputProvenance["recorded"] = strings.Repeat("x", 257)
	if _, err := DecomposePortfolio(context.Background(), decompositionBook(), staticModel{m}); !errors.Is(err, v1.ErrDecompositionTooLarge) {
		t.Fatalf("oversized parameter value accepted: %v", err)
	}
}
