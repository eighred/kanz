package compute

import (
	"context"
	decutil "github.com/eighred/kanz/internal/dec"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

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
	// ONLY TermsResolved REPRICES. The other three answers all mean "do not
	// shock this position", and a scenario has nowhere to put a coverage record
	// — its output is a shocked MeasureSet, whose measures carry their own
	// coverage from the same providers. So the distinction TermsResolution draws
	// is deliberately not consumed here; it is consumed where it changes an
	// answer somebody reads.
	spec, res := rv.providers.Terms.BondTerms(ctx, instrumentID, asOf)
	if res != TermsResolved {
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
	newAmount := decutil.Float64Or(baseMV.GetAmount(), 0) * ratio
	return &commonpb.Money{Amount: floatToDecimal(newAmount, revalMoneyExp), CurrencyCode: baseMV.GetCurrencyCode()}, true
}
