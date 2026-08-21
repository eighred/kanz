package compute

import (
	decutil "github.com/eighred/kanz/internal/dec"
	"math"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
)

// hhiExponent is the Decimal scale of the emitted HHI: a dimensionless ratio in
// [1/n, 1], so a fractional scale (not the -2 money scale VaR/ES use).
const hhiExponent int32 = -4

// HHI is the Herfindahl-Hirschman Index of position concentration: Σ wᵢ² where
// wᵢ is each position's share of GROSS exposure (|MarketValue| / Σ|MarketValue|),
// over positions in the portfolio base currency (the RISK-07 same-currency
// convention). Other-currency positions are excluded and reported as
// v1.QualityFlagCurrencyExcluded on the response — and here the exclusion bites
// hardest: HHI is a RATIO, so dropping positions raises it towards 1 as often as
// it lowers it, and a book that looks maximally concentrated may just be a book
// measured one position at a time. Never read an HHI off a flagged response
// (#257). Bounds: 1/n ≤ HHI ≤ 1 — 1
// is a single-position book (maximally concentrated), 1/n is n equal positions
// (maximally diversified). A zero-gross / empty portfolio yields the zero-value
// measure (the MeasureFunc never-error contract). Needs no market data, so it is
// registered unconditionally in DefaultRegistry and is served even in the
// no-price-store fallback.
func HHI(p *domain.Portfolio) v1.Measure {
	base := p.BaseCurrency()
	var sumSq, gross float64
	for _, pos := range p.Positions() {
		if !pos.InBaseCurrency(base) {
			continue
		}
		v := math.Abs(decutil.Float64Or(pos.MarketValue.Amount, 0))
		sumSq += v * v
		gross += v
	}
	if gross == 0 {
		return v1.Measure{Name: MeasureHHI, Value: zeroDecimal()}
	}
	hhi := sumSq / (gross * gross)
	return v1.Measure{Name: MeasureHHI, Value: floatToDecimal(hhi, hhiExponent)}
}
