package compute

import (
	"context"
	"math"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/eighred/kanz/internal/dec"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/factormodel"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// DecomposePortfolio resolves the same provider used by RegisterFactorRisk at
// the frozen book's actual time. A provider miss is a refusal, never a clean
// empty decomposition. Only admitted positions are sent to Model.Decompose.
func DecomposePortfolio(ctx context.Context, p *domain.Portfolio, provider ModelProvider) (v1.RiskDecomposition, error) {
	if err := ctx.Err(); err != nil {
		return v1.RiskDecomposition{}, err
	}
	if p == nil || provider == nil || p.AsOf().IsZero() || !decompositionKey(string(p.BaseCurrency())) {
		return v1.RiskDecomposition{}, v1.ErrDecompositionUnavailable
	}
	if !decompositionKey(string(p.ID())) || p.PositionCount() > v1.MaxDecompositionInstruments {
		return v1.RiskDecomposition{}, v1.ErrDecompositionTooLarge
	}
	positions := p.Positions()
	if len(positions) > v1.MaxDecompositionInstruments {
		return v1.RiskDecomposition{}, v1.ErrDecompositionTooLarge
	}
	model, ok := provider.Model(ctx, p.AsOf())
	if err := ctx.Err(); err != nil {
		return v1.RiskDecomposition{}, err
	}
	if !ok || model == nil || model.AsOf.IsZero() || model.AsOf.After(p.AsOf()) || !decompositionKey(model.ModelID) || !decompositionKey(factorInputDigest(model)) {
		return v1.RiskDecomposition{}, v1.ErrDecompositionUnavailable
	}
	if len(model.Factors) > v1.MaxDecompositionFactors {
		return v1.RiskDecomposition{}, v1.ErrDecompositionTooLarge
	}
	if len(model.Factors) == 0 {
		return v1.RiskDecomposition{}, v1.ErrDecompositionUnavailable
	}
	seen := make(map[string]bool, len(model.Factors))
	for _, factor := range model.Factors {
		if !decompositionKey(factor.Name) {
			return v1.RiskDecomposition{}, v1.ErrDecompositionTooLarge
		}
		if seen[factor.Name] {
			return v1.RiskDecomposition{}, v1.ErrDecompositionUnavailable
		}
		seen[factor.Name] = true
	}
	var coverage Coverage
	values := make(map[string]float64, len(positions))
	for _, pos := range positions { // Portfolio.Positions is in instrument-ID order.
		if !decompositionKey(string(pos.InstrumentID)) {
			return v1.RiskDecomposition{}, v1.ErrDecompositionTooLarge
		}
		if !pos.InBaseCurrency(p.BaseCurrency()) {
			coverage.Exclude(pos.InstrumentID, "currency_unresolved")
			continue
		}
		value, valid := dec.Float64(pos.MarketValue.GetAmount())
		if !valid {
			coverage.Exclude(pos.InstrumentID, "market_value_unresolved")
			continue
		}
		if _, exists := model.Loading(string(pos.InstrumentID)); !exists {
			coverage.Exclude(pos.InstrumentID, SkipNotInModel)
			continue
		}
		coverage.Contributed++
		values[string(pos.InstrumentID)] = value
	}
	out, err := decomposeModel(model, values)
	if err != nil {
		return v1.RiskDecomposition{}, err
	}
	out.Coverage = coverage.Result()
	// Reuse the scalar family's fitted-instance provenance, without adding a
	// VaR confidence to a standard-deviation decomposition. Bound metadata too.
	out.Provenance = factorProvenance(model, "")
	if len(out.Provenance.Params) > v1.MaxDecompositionParams {
		return v1.RiskDecomposition{}, v1.ErrDecompositionTooLarge
	}
	for key, value := range out.Provenance.Params {
		if !decompositionKey(key) || !decompositionKey(value) {
			return v1.RiskDecomposition{}, v1.ErrDecompositionTooLarge
		}
	}
	return out, nil
}

func decomposeModel(model *factormodel.Model, values map[string]float64) (v1.RiskDecomposition, error) {
	d := model.Decompose(values)
	var out v1.RiskDecomposition
	var err error
	convert := func(value float64) *commonpb.Decimal {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			err = v1.ErrDecompositionUnavailable
			return nil
		}
		valueDecimal, ok := dec.ParseProtoExact(strconv.FormatFloat(value, 'g', -1, 64))
		if !ok || !dec.InDomain(valueDecimal) {
			err = v1.ErrDecompositionUnavailable
			return nil
		}
		return valueDecimal
	}
	out.Total, out.Systematic = convert(d.Total), convert(d.Systematic)
	out.Specific, out.SpecificComponent = convert(d.Specific), convert(d.SpecificRiskContribution)
	// FactorExposure retains the complete model shape even when total risk is
	// zero. Decompose's contribution map is empty on that early-return path.
	keys := make([]string, 0, len(d.FactorExposure))
	for key := range d.FactorExposure {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out.ByFactor = append(out.ByFactor, v1.FactorRiskContribution{Key: key, Exposure: convert(d.FactorExposure[key]), Component: convert(d.FactorRiskContribution[key])})
	}
	keys = keys[:0]
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out.ByInstrument = append(out.ByInstrument, v1.InstrumentRiskContribution{InstrumentID: v1.InstrumentID(key), Marginal: convert(d.MarginalRisk[key]), Component: convert(d.ComponentRisk[key])})
	}
	if err != nil {
		return v1.RiskDecomposition{}, err
	}
	return out, nil
}

func decompositionKey(key string) bool {
	return key != "" && len(key) <= v1.MaxDecompositionKeyBytes && utf8.ValidString(key)
}
