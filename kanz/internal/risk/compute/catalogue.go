package compute

import (
	"sort"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
)

// EVERY MEASURE THIS PLATFORM IMPLEMENTS, WHETHER OR NOT AN ENGINE SERVES IT
// (#509).
//
// # Why this exists
//
// A measure that is implemented but never registered is not an error anywhere.
// internal/risk/engine.filterMeasures documents its own behaviour — "unknown
// names are dropped" — so a client that asks for DV01 on a bond book receives
// 200 with DV01 absent. That is INDISTINGUISHABLE from a portfolio holding no
// bonds, and from a computation that was attempted and yielded nothing.
//
// The registry can only answer "what am I serving". It cannot answer "what
// should I be serving", because an unregistered measure leaves no trace in it.
// This is the other half of that question, and MeasurePosture subtracts one from
// the other.
//
// # The zeroes are the point
//
// The same argument as benchmarks.Inventory: a catalogue listing only the
// registered measures would climb from nothing to nothing and always read as
// full coverage. When this was written the risk engine served EIGHT of the
// twenty-six below — DefaultRegistry plus varmodel.Register — and the eighteen
// dark ones included every Greek, every fixed-income measure and the whole XVA
// family. None of that was visible from any signal the platform had.
//
// # Hand-maintained, and UNLIKE Inventory that hole is closed
//
// Nothing in Go enumerates constants, so this is a list. benchmarks.Inventory
// carries the same weakness and calls it "the known weakness" — an analytic
// added without a line there is invisible to the count, in the direction that
// flatters.
//
// Here it is closed by a guard rather than by discipline:
// test/arch/measure_catalogue_test.go parses every `Measure… v1.MeasureName`
// constant under internal/risk and fails if one is missing from this list, or if
// this list names one that no longer exists. Adding a measure and forgetting
// this file is a red build, not a quieter metric.

// MeasureFamily groups measures by the seam that registers them, so a dark
// family points at one missing wiring rather than at n missing measures.
type MeasureFamily string

const (
	FamilyExposure    MeasureFamily = "exposure"     // DefaultRegistry
	FamilyTailRisk    MeasureFamily = "tail_risk"    // varmodel.Register
	FamilyGreeks      MeasureFamily = "greeks"       // RegisterGreeks
	FamilyFixedIncome MeasureFamily = "fixed_income" // RegisterFIRisk
	FamilyFactor      MeasureFamily = "factor"       // RegisterFactorRisk
	FamilyLiquidity   MeasureFamily = "liquidity"    // RegisterLiquidityRisk
	FamilyStructured  MeasureFamily = "structured"   // RegisterStructuredRisk
	FamilyXVA         MeasureFamily = "xva"          // RegisterXVA
	FamilyMargin      MeasureFamily = "margin"       // RegisterMarginRisk
)

// catalogue maps every implemented measure to the family whose seam registers
// it. Keep sorted within a family; the guard does not care about order, readers
// do.
var catalogue = map[v1.MeasureName]MeasureFamily{
	// DefaultRegistry — served by every engine, no provider needed.
	MeasureGrossExposure: FamilyExposure,
	MeasureNetExposure:   FamilyExposure,
	MeasureHHI:           FamilyExposure,
	// Delta is in DefaultRegistry as a PLACEHOLDER (net exposure) and is
	// OVERWRITTEN by RegisterGreeks with the pricing-derived value. It is
	// catalogued under greeks because that is where its real implementation
	// lives.
	//
	// CORRECTION (2026-08-16). This comment used to end "— so a served
	// placeholder Delta does not make the Greeks family look live", and that was
	// FALSE. Dark() asks the registry whether a NAME is registered; it cannot ask
	// which implementation registered it. DefaultRegistry registers Delta, so
	// kanz_risk_measure_live{measure="Delta",family="greeks"} is 1 on an engine
	// that serves no pricing-derived Greek at all, and the greeks family reads
	// 1-of-5 rather than 0-of-5.
	//
	// The catalogue is not the place to fix that: provenance is a property of the
	// registration, and Registry deliberately holds only a name and a MeasureFunc
	// (its own doc: "Quants plug in real models by calling Register"). Recording
	// which implementation won would be a second concept in the registry to keep
	// right, to correct one measure's reading. So the honest move is to say what
	// is true here and in the metric's Help, and MeasurePosture does.
	MeasureDelta: FamilyGreeks,

	// varmodel.Register — needs a ReturnsProvider.
	MeasureVaR99:             FamilyTailRisk,
	MeasureES99:              FamilyTailRisk,
	MeasureMaxDrawdown:       FamilyTailRisk,
	MeasureMaxDrawdownAmount: FamilyTailRisk,

	// RegisterGreeks — needs Terms, Spot, Vol and a discount curve.
	MeasureGamma: FamilyGreeks,
	MeasureVega:  FamilyGreeks,
	MeasureTheta: FamilyGreeks,
	MeasureRho:   FamilyGreeks,

	// RegisterFIRisk — needs BondTerms and a discount curve.
	MeasureDV01:           FamilyFixedIncome,
	MeasureDuration:       FamilyFixedIncome,
	MeasureConvexity:      FamilyFixedIncome,
	MeasureSpreadDuration: FamilyFixedIncome,

	// RegisterFactorRisk — needs a fitted factor model.
	MeasureFactorVaR99:    FamilyFactor,
	MeasureSystematicRisk: FamilyFactor,
	MeasureSpecificRisk:   FamilyFactor,

	// RegisterLiquidityRisk — needs per-instrument ADV/spread.
	MeasureLVaR99:             FamilyLiquidity,
	MeasureLiquidationHorizon: FamilyLiquidity,

	// RegisterStructuredRisk — needs StructuredTerms (the ContractTerms
	// `structured` variant, #572) and a discount curve.
	MeasureStructDuration:  FamilyStructured,
	MeasureStructConvexity: FamilyStructured,
	MeasureStructWAL:       FamilyStructured,

	// RegisterXVA — needs exposure profiles and counterparty credit curves.
	MeasureCVA: FamilyXVA,
	MeasurePFE: FamilyXVA,

	// RegisterMarginRisk — needs a MarginProvider: the venue accounts backing a
	// portfolio, and the exchange's own liquidation price for each open position
	// paired with a reference price (#408 control 4).
	MeasureLiquidationProximity: FamilyMargin,
}

// Catalogue returns every implemented measure with its family, in stable
// name order.
func Catalogue() []CataloguedMeasure {
	out := make([]CataloguedMeasure, 0, len(catalogue))
	for name, family := range catalogue {
		out = append(out, CataloguedMeasure{Name: name, Family: family})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CataloguedMeasure is one implemented measure and the family it belongs to.
type CataloguedMeasure struct {
	Name   v1.MeasureName
	Family MeasureFamily
}

// Dark returns the catalogued measures a registry does NOT serve, in stable
// name order.
//
// This is the whole question the registry cannot answer about itself: an
// unregistered measure leaves no trace in it, so "what is missing" has to be
// asked from outside.
func Dark(r *Registry) []CataloguedMeasure {
	served := make(map[v1.MeasureName]bool, len(r.funcs))
	for _, n := range r.Names() {
		served[n] = true
	}
	var out []CataloguedMeasure
	for _, m := range Catalogue() {
		if !served[m.Name] {
			out = append(out, m)
		}
	}
	return out
}
