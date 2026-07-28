package compute

import (
	"context"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
)

// Full-revaluation of bond positions under a yield-curve shift (FI-01e) — the
// fixed-income sibling of the option Revaluer. Where a linear scenario cannot
// express a rate move at all (a bond's MarketValue has no price-shock input), a
// curve shift reprices each bond off the shifted discount curve, capturing
// duration AND convexity. Wired with the same FIProviders the FI risk measures
// use, so scenario repricing and risk reporting price off one curve.

// BondRevaluer reprices bond positions under a curve.Shift. A non-bond position
// (or one missing terms/curve) returns ok=false so the scenario leaves it
// unshocked — a curve shift does not move a cash equity.
type BondRevaluer struct{ providers FIProviders }

// NewBondRevaluer wraps the FI providers.
func NewBondRevaluer(p FIProviders) *BondRevaluer { return &BondRevaluer{providers: p} }

// RevalueBond returns baseMV repriced under the curve shift: baseMV ×
// (P_shifted / P_base), preserving position size and sign. ok=false ⇒ leave the
// position unshocked.
func (rv *BondRevaluer) RevalueBond(ctx context.Context, instrumentID string, asOf time.Time, baseMV *commonpb.Money, shift curve.Shift) (*commonpb.Money, bool) {
	if rv.providers.Terms == nil || rv.providers.Curve == nil || baseMV == nil || shift == nil {
		return nil, false
	}
	spec, ok := rv.providers.Terms.BondTerms(ctx, instrumentID, asOf)
	if !ok {
		return nil, false
	}
	c, ok := rv.providers.Curve.Curve(ctx, spec.Currency, asOf)
	if !ok || c == nil {
		return nil, false
	}
	bond := pricing.Bond{
		Face:       spec.Face,
		CouponRate: spec.CouponRate,
		Frequency:  spec.Frequency,
		Issue:      spec.Issue,
		Maturity:   spec.Maturity,
		DayCount:   spec.DayCount,
	}
	base := bond.PriceFromCurve(asOf, c)
	if base <= 0 {
		return nil, false
	}
	shocked := bond.PriceFromCurve(asOf, shift.Apply(c))
	ratio := shocked / base
	newAmount := decimalToFloat(baseMV.GetAmount()) * ratio
	return &commonpb.Money{Amount: floatToDecimal(newAmount, revalMoneyExp), CurrencyCode: baseMV.GetCurrencyCode()}, true
}
