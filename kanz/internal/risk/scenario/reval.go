package scenario

import (
	"context"
	"math/big"
	"time"

	"github.com/eighred/kanz/internal/dec"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/compute/factor"
	"github.com/eighred/kanz/internal/risk/domain"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// Full-revaluation scenarios (DERIV-01e). EvaluateReval is the sibling of
// Evaluate that reprices OPTION positions through Black-Scholes/binomial under
// the scenario's price + vol shocks, instead of scaling their MarketValue
// linearly — so a long-gamma book shows its convexity and a vol shock shows its
// vega. Non-option positions keep the linear treatment, and a portfolio with no
// reval-able positions yields exactly what Evaluate would.
//
// It is a separate entry point, not a change to Evaluate, so the existing linear
// scenario path (and its callers) are untouched.

// Revaluer reprices an option position under shocks. compute.Revaluer satisfies
// it; the interface lives here so scenario does not force a particular
// implementation. The typed disposition separates a known linear instrument
// from an unassessed position whose approximation must carry an exclusion.
type Revaluer interface {
	RevalueOption(ctx context.Context, instrumentID string, asOf time.Time, baseMV *commonpb.Money, shocks compute.RevalShocks) compute.RevalResult
}

// EvaluateReval applies shocks to a deep clone of p — repricing option positions
// via reval and scaling the rest linearly — then computes measures against the
// revalued state. p is not mutated. A nil reval falls back to the fully linear
// Evaluate.
//
// # It is now a spelling of Evaluate(WithRevaluer(reval)), not a second path
//
// It used to assemble its own coverage and call cloneWithReval directly, and
// that is exactly how the two paths came to disagree about VolShock: this one
// priced the bump through RevalShocks.GlobalVolBump, the linear one no-opped it
// and recorded nothing, and only the linear one had a caller — so the reachable
// answer to a +15 vol-point stress was the unshocked book with a clean coverage
// record (#1035). One entry point means a new shock class is considered once.
//
// It returns the SAME shock-application coverage Evaluate does, and for the same
// reason: the revaluation path resolves each position's sector through the classifier, so
// this path had the identical silent degradation (#640). It carries no caller
// today — EvaluateReval is a tracked dark seam in
// test/arch/no_dark_measure_seam_test.go, blocked on a Revaluer — and wiring it
// with the coverage already threaded is what stops the defect being reintroduced
// on the commit that lights it up.
func EvaluateReval(p *domain.Portfolio, shocks []v1.ScenarioShock, registry *compute.Registry, reval Revaluer, opts ...Option) (*domain.MeasureSet, v1.InputCoverage) {
	if reval == nil {
		return Evaluate(p, shocks, registry, opts...)
	}
	// A FRESH SLICE, never append(opts, ...): opts belongs to the caller and
	// appending in place would write WithRevaluer into its spare capacity, which
	// a caller reusing one options slice across portfolios would carry silently.
	withReval := make([]Option, 0, len(opts)+1)
	withReval = append(withReval, opts...)
	withReval = append(withReval, WithRevaluer(reval))
	return Evaluate(p, shocks, registry, withReval...)
}

// cloneWithReval deep-copies p and applies the shocks: each option position is
// repriced under its aggregated shocks; each other position is scaled by its
// linear price fraction.
func cloneWithReval(p *domain.Portfolio, shocks []v1.ScenarioShock, reval Revaluer, cfg evalConfig, cov *shockCoverage) *domain.Portfolio {
	cp := domain.NewPortfolio(p.ID(), p.BaseCurrency())
	cp.SetAggregate(domain.AggregateUpdate{
		AsOf:             p.AsOf(),
		DisplayName:      p.DisplayName(),
		BaseCurrency:     p.BaseCurrency(),
		CashBalance:      p.CashBalance(),
		TotalMarketValue: p.TotalMarketValue(),
		PositionCount:    p.PositionCount(),
	})
	for _, pos := range p.Positions() {
		cp.SetPosition(pos)
	}

	parallelFrac, globalVol, volByUnderlying := collectGlobalShocks(shocks)
	// ONCE, BEFORE THE LOOP: no classifier is a property of the evaluation, not
	// of any holding, so it is one whole-book exclusion. Recording it per
	// position would make a deployment-wide gap look like a per-instrument data
	// hole and would scale the count with the book size.
	needsSector := hasSectorShock(shocks)
	if cfg.classifier == nil && needsSector {
		cov.noClassifierWired()
	}
	ctx := context.Background()
	asOf := p.AsOf()
	for _, pos := range cp.Positions() {
		var sector *factor.Sector
		if needsSector && cfg.classifier != nil {
			cl, ok := cfg.classifier.Classify(ctx, string(pos.InstrumentID), asOf)
			if ok {
				sector = &cl.Sector
				cov.resolved(pos.InstrumentID)
			} else {
				cov.unclassified(pos.InstrumentID)
			}
		}
		adjustments, instrumentFrac, sectorFrac := matchedPriceShocks(shocks, pos.InstrumentID, sector)
		priceFrac := parallelFrac + instrumentFrac + sectorFrac
		rs := compute.RevalShocks{PriceFrac: priceFrac, GlobalVolBump: globalVol, VolByUnderlying: volByUnderlying}
		result := reval.RevalueOption(ctx, string(pos.InstrumentID), asOf, pos.MarketValue, rs)
		if result.Status == compute.RevalPriced && result.Value != nil {
			pos.MarketValue = result.Value
			cp.SetPosition(pos)
			continue
		}
		if result.Status != compute.RevalKnownLinear {
			reason := result.Reason
			if reason == "" {
				reason = compute.SkipInvalidPricing
			}
			cov.revaluationUnassessed(pos.InstrumentID, reason)
		}
		// Keep every holding in the approximation. Coverage prevents an
		// unassessed option/classification from looking like a complete stress.
		if len(adjustments) > 0 {
			value, ok := linearValue(pos.MarketValue, adjustments)
			if !ok {
				cov.revaluationUnassessed(pos.InstrumentID, compute.SkipInvalidPricing)
				continue
			}
			pos.MarketValue = value
			cp.SetPosition(pos)
		}
	}
	return cp
}

func (s *shockCoverage) revaluationUnassessed(id domain.InstrumentID, reason string) {
	if excluded, seen := s.seen[id]; seen {
		if excluded {
			return
		}
		// A resolved sector does not certify option valuation. One position
		// remains one exclusion, rather than a contribution plus an exclusion.
		s.cov.Contributed--
	}
	s.seen[id] = true
	s.cov.Exclude(id, reason)
}

// collectGlobalShocks sums the book-wide price fraction (ParallelShift) and vol
// bumps (VolShock) from the shock list.
func collectGlobalShocks(shocks []v1.ScenarioShock) (parallelFrac, globalVol float64, volByUnderlying map[string]float64) {
	volByUnderlying = map[string]float64{}
	for _, sh := range shocks {
		switch s := sh.(type) {
		case v1.ParallelShift:
			parallelFrac += dec.Float64Or(s.Pct, 0)
		case v1.VolShock:
			if s.UnderlyingID == "" {
				globalVol += dec.Float64Or(s.AbsBump, 0)
			} else {
				volByUnderlying[string(s.UnderlyingID)] += dec.Float64Or(s.AbsBump, 0)
			}
		}
	}
	return parallelFrac, globalVol, volByUnderlying
}

// Preserve the ordered Decimal shocks for linear positions and the existing
// additive instrument and sector inputs for option pricing.
func matchedPriceShocks(shocks []v1.ScenarioShock, id domain.InstrumentID, sector *factor.Sector) (adjustments []*commonpb.Decimal, instrumentFrac, sectorFrac float64) {
	for _, sh := range shocks {
		var pct *commonpb.Decimal
		switch s := sh.(type) {
		case v1.ParallelShift:
			pct = s.Pct
		case v1.PriceShock:
			if s.InstrumentID == id {
				pct = s.Pct
				instrumentFrac += dec.Float64Or(pct, 0)
			}
		case v1.SectorShock:
			if sector != nil && sector.Taxonomy == s.Taxonomy && sector.Code == s.Code {
				pct = s.Pct
				sectorFrac += dec.Float64Or(pct, 0)
			}
		}
		if pct != nil && pct.Coefficient != 0 {
			adjustments = append(adjustments, pct)
		}
	}
	return adjustments, instrumentFrac, sectorFrac
}

// Each linear shock scales the latest market value. Keep every factor exact
// and require each resulting position valuation to fit the Decimal contract.
func linearValue(m *commonpb.Money, adjustments []*commonpb.Decimal) (*commonpb.Money, bool) {
	if m.GetAmount() == nil {
		return nil, false
	}
	value, ok := dec.FromProtoChecked(m.Amount)
	if !ok {
		return nil, false
	}
	one := big.NewRat(1, 1)
	for _, pct := range adjustments {
		scale := dec.FromProto(pct) // Evaluate preflights every magnitude.
		value.Mul(value, scale.Add(scale, one))
		candidate := compute.ShockMoney(m, pct)
		if candidate.GetAmount() != nil {
			if actual, ok := dec.FromProtoChecked(candidate.Amount); ok && actual.Cmp(value) == 0 {
				m = candidate
				continue
			}
		}
		// The canonical arithmetic may coarsen a coefficient. Materialize the
		// exact value if possible; an approximation cannot certify coverage.
		amount, ok := dec.ToProtoExact(value)
		if !ok {
			return nil, false
		}
		m = &commonpb.Money{Amount: amount, CurrencyCode: m.CurrencyCode}
	}
	return m, true
}

// hasSectorShock reports whether the batch contains a SectorShock at all. With
// none, no position's sector is ever consulted, so nothing is missing and the
// coverage stays empty — a price-and-vol scenario must not be refused for want
// of a classifier it does not use.
func hasSectorShock(shocks []v1.ScenarioShock) bool {
	for _, sh := range shocks {
		if _, ok := sh.(v1.SectorShock); ok {
			return true
		}
	}
	return false
}

// fracDecimal encodes a model-derived factor return for factor revaluation.
func fracDecimal(f float64) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: int64(f * 1e6), Exponent: -6}
}
