package compute

import (
	"math"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	v1 "github.com/kanz-eng/kanz/internal/risk/api/v1"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// hhiExponent is the Decimal scale of the emitted HHI: a dimensionless ratio in
// [1/n, 1], so a fractional scale (not the -2 money scale VaR/ES use).
const hhiExponent int32 = -4

// HHI is the Herfindahl-Hirschman Index of position concentration: Σ wᵢ² where
// wᵢ is each position's share of GROSS exposure (|MarketValue| / Σ|MarketValue|),
// over positions in the portfolio base currency (the RISK-07 same-currency
// convention; other-currency positions are skipped). Bounds: 1/n ≤ HHI ≤ 1 — 1
// is a single-position book (maximally concentrated), 1/n is n equal positions
// (maximally diversified). A zero-gross / empty portfolio yields the zero-value
// measure (the MeasureFunc never-error contract). Needs no market data, so it is
// registered unconditionally in DefaultRegistry and is served even in the
// no-price-store fallback.
func HHI(p *domain.Portfolio) v1.Measure {
	base := string(p.BaseCurrency())
	var sumSq, gross float64
	for _, pos := range p.Positions() {
		if pos.MarketValue == nil || pos.MarketValue.CurrencyCode != base {
			continue
		}
		v := math.Abs(decimalToFloat(pos.MarketValue.Amount))
		sumSq += v * v
		gross += v
	}
	if gross == 0 {
		return v1.Measure{Name: MeasureHHI, Value: &commonpb.Decimal{Coefficient: 0, Exponent: 0}}
	}
	hhi := sumSq / (gross * gross)
	return v1.Measure{Name: MeasureHHI, Value: floatToDecimal(hhi, hhiExponent)}
}
