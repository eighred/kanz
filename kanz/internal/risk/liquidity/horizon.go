// Package liquidity is the LIQ-01 liquidity-risk layer: the liquidation-horizon
// model (how long to unwind a book), the Almgren-Chriss-style market-impact /
// liquidation cost, and the liquidity-adjusted VaR that widens VaR by that cost.
// It is the liquidity dimension the market.v1 quote feed already carries — a
// spread and a top-of-book depth — but nothing modelled until now (ROI #30).
//
// # The seam mirrors the FI / Greek enrichment
//
// Liquidity needs data a Position does not carry — the instrument's average
// daily volume and typical spread. As with the FI-01d / DERIV-01d measures, the
// models close over a Provider (the float working shape behind
// reference.v1.LiquidityProfile, the FI-01 BondSpec precedent) resolved per
// position at the portfolio's point-in-time asOf. The compute layer wires these
// into the RISK-07 registry (LIQ-01d) without changing the MeasureFunc contract.
//
// # Currency convention
//
// Like the RISK-07 baseline measures, only positions whose MarketValue is in the
// portfolio base currency are summed (no FX layer in the risk module); other-
// currency positions are skipped, surfaced as a quality flag a layer up.
package liquidity

import (
	"context"
	"math"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/domain"
)

// LiquiditySpec is the per-instrument liquidity reference the horizon/cost models
// read — the float working shape behind reference.v1.LiquidityProfile, resolved
// by a Provider. ADV is in the instrument's natural units (shares/contracts);
// Spread is a fraction of price (0.0005 = 5bps); ParticipationRate is an optional
// per-instrument override of the model default (0 ⇒ use the default).
type LiquiditySpec struct {
	ADV               float64
	Spread            float64
	ParticipationRate float64
}

// Provider resolves an instrument's liquidity reference as of a point in time —
// the LIQ-01a mirror of the FI BondTermsProvider. ok=false ⇒ no liquidity data
// for the instrument (it is excluded from the liquidity measures, surfaced as a
// quality concern a layer up rather than silently assumed liquid).
type Provider interface {
	Liquidity(ctx context.Context, instrumentID string, asOf time.Time) (LiquiditySpec, bool)
}

// Default model parameters.
const (
	// DefaultParticipationRate is the fraction of ADV the desk assumes it can
	// trade per day when unwinding a name (20% is a conventional cap that keeps
	// estimated impact modest).
	DefaultParticipationRate = 0.20
	// DefaultImpactCoeff scales the horizon-widening (Almgren-Chriss temporary
	// impact) term of the liquidation cost. 1.0 ⇒ cost = half-spread × √horizon;
	// 0 ⇒ the pure exogenous-spread cost with no horizon widening.
	DefaultImpactCoeff = 1.0
)

// Model holds the desk-level liquidation assumptions. The zero value is usable
// (participation falls back to DefaultParticipationRate; ImpactCoeff=0 is the
// conservative no-widening floor); DefaultModel is the calibrated default.
type Model struct {
	ParticipationRate float64
	ImpactCoeff       float64
}

// DefaultModel returns the calibrated liquidation model.
func DefaultModel() Model {
	return Model{ParticipationRate: DefaultParticipationRate, ImpactCoeff: DefaultImpactCoeff}
}

// participation resolves the per-name participation rate: a positive per-spec
// override, else the model default, else the package default.
func (m Model) participation(specRate float64) float64 {
	if specRate > 0 {
		return specRate
	}
	if m.ParticipationRate > 0 {
		return m.ParticipationRate
	}
	return DefaultParticipationRate
}

// DaysToLiquidate is the number of trading days to unwind |quantity| units at the
// model participation rate: |quantity| ÷ (participation × ADV). ok=false (and
// +Inf days) when ADV or the participation rate is non-positive — the instrument
// cannot be liquidated under these assumptions (LIQ-01b ADV-zero handling); the
// caller flags it illiquid rather than reporting a finite horizon. Monotonic
// increasing in |quantity| and decreasing in ADV.
func (m Model) DaysToLiquidate(quantity float64, s LiquiditySpec) (float64, bool) {
	rate := m.participation(s.ParticipationRate) * s.ADV
	if rate <= 0 {
		return math.Inf(1), false
	}
	return math.Abs(quantity) / rate, true
}

// PositionHorizon is one position's liquidation horizon and weight.
type PositionHorizon struct {
	InstrumentID string
	// Days is the days-to-liquidate; +Inf when Liquid is false.
	Days float64
	// Liquid is false when ADV is unavailable/zero (the horizon is undefined).
	Liquid bool
	// Notional is |MarketValue| in the portfolio base currency.
	Notional float64
}

// Profile is a portfolio's liquidation profile: the per-position horizons plus
// the aggregate summary. MaxDays is the full-book horizon (the slowest liquid
// name gates a parallel unwind); WeightedDays is the notional-weighted average
// horizon over liquid names; IlliquidNotional is the notional that cannot be
// liquidated at all (ADV≤0).
type Profile struct {
	Positions        []PositionHorizon
	MaxDays          float64
	WeightedDays     float64
	IlliquidNotional float64
}

// LiquidationProfile builds p's liquidation profile from the provider, resolving
// each position's liquidity at p.AsOf(). Only base-currency positions are
// included (the RISK-07 same-currency convention). A position with no liquidity
// data is skipped entirely; one with zero ADV is flagged illiquid (counted in
// IlliquidNotional, excluded from the weighted/max horizon over liquid names).
func (m Model) LiquidationProfile(ctx context.Context, p *domain.Portfolio, provider Provider) Profile {
	base := string(p.BaseCurrency())
	asOf := p.AsOf()
	var prof Profile
	var wSum, nSum float64
	for _, pos := range p.Positions() {
		if pos.MarketValue == nil || pos.MarketValue.CurrencyCode != base {
			continue
		}
		spec, ok := provider.Liquidity(ctx, string(pos.InstrumentID), asOf)
		if !ok {
			continue
		}
		notional := math.Abs(decimalToFloat(pos.MarketValue.GetAmount()))
		days, liquid := m.DaysToLiquidate(decimalToFloat(pos.Quantity), spec)
		prof.Positions = append(prof.Positions, PositionHorizon{
			InstrumentID: string(pos.InstrumentID),
			Days:         days,
			Liquid:       liquid,
			Notional:     notional,
		})
		if !liquid {
			prof.IlliquidNotional += notional
			continue
		}
		if days > prof.MaxDays {
			prof.MaxDays = days
		}
		wSum += notional * days
		nSum += notional
	}
	if nSum > 0 {
		prof.WeightedDays = wSum / nSum
	}
	return prof
}

// decimalToFloat is the package's Decimal→float bridge (compute's equivalent is
// package-private). Liquidity figures are float-domain statistics.
func decimalToFloat(d *commonpb.Decimal) float64 {
	if d == nil {
		return 0
	}
	return float64(d.Coefficient) * math.Pow10(int(d.Exponent))
}
