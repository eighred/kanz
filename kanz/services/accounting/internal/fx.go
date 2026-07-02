package accounting

import "math/big"

// Multi-currency valuation support (PARITY-05f, the IBOR-01d carried-forward FX
// seam). A book holds positions priced in their own reference currency and cash
// in several currencies; NAV in a single reporting currency needs a per-currency
// FX conversion, and the change in those rates between two valuations is the
// real "fx" attribution driver (previously supplied as an opaque zero).

// FXConverter gives the rate to convert an amount from a quoted currency into
// the NAV reporting currency at a point in time: units of reporting per 1 unit
// of `from`. The reporting currency itself is always 1. ok=false ⇒ no rate for
// that currency, and a valuation that needs it must fail loudly (the same
// completeness discipline as a missing price) rather than silently mis-value.
type FXConverter interface {
	Rate(from string) (*big.Rat, bool)
}

// FXTable is a point-in-time rate table: currency → units of the reporting
// currency per 1 unit of that currency, with the reporting currency implicitly
// 1. The composition root builds it from the live FX feed (the carried-forward
// seam) as of the valuation time; tests build it inline.
type FXTable struct {
	reporting string
	rates     map[string]*big.Rat
}

// NewFXTable builds a rate table quoting into reporting. rates maps each foreign
// currency to units-of-reporting-per-unit; the reporting currency need not be
// present (it is always 1). Values are copied, so the caller's map is not
// aliased.
func NewFXTable(reporting string, rates map[string]*big.Rat) *FXTable {
	cp := make(map[string]*big.Rat, len(rates))
	for k, v := range rates {
		if v != nil {
			cp[k] = new(big.Rat).Set(v)
		}
	}
	return &FXTable{reporting: reporting, rates: cp}
}

// Rate returns units of reporting per 1 unit of `from`. The reporting currency
// (and the empty currency, treated as domestic) is 1; a foreign currency with no
// configured rate is (nil, false).
func (t *FXTable) Rate(from string) (*big.Rat, bool) {
	if from == "" || from == t.reporting {
		return big.NewRat(1, 1), true
	}
	if r, ok := t.rates[from]; ok {
		return new(big.Rat).Set(r), true
	}
	return nil, false
}

// Compile-time assertion.
var _ FXConverter = (*FXTable)(nil)

// InstrumentCurrency is the per-instrument reference-currency join: the currency
// each instrument's price is quoted in. A missing entry is treated as the
// reporting currency (a domestic instrument), so a single-currency book needs no
// map at all. The composition root populates it from the MASTER security master.
type InstrumentCurrency map[string]string

// Of returns the reference currency of instrument, defaulting to reporting when
// unmapped or blank.
func (m InstrumentCurrency) Of(instrument, reporting string) string {
	if m != nil {
		if c, ok := m[instrument]; ok && c != "" {
			return c
		}
	}
	return reporting
}

// FXPnL is the currency-revaluation P&L over a period: the (prior) per-currency
// exposure revalued at the CHANGE in its FX rate — Σ exposure_ccy × (rateₙ −
// rateₙ₋₁). This is the real-rate "fx" driver the Attribute identity expects; the
// residual local-currency mark-to-market stays in "price" (Attribute subtracts
// fx out of it). The reporting currency contributes 0 (its rate is 1 at both
// ends), so a single-currency book yields fx = 0 exactly as before.
//
// Convention: exposure is the OPENING book (prior NAV's LocalExposure), so fx is
// "the currency move applied to what you held coming into the period". A currency
// present in the exposure but missing a rate on either side is skipped (it cannot
// be revalued) rather than fabricating a move.
func FXPnL(exposure map[string]*big.Rat, priorFX, currentFX FXConverter) *big.Rat {
	fx := new(big.Rat)
	if exposure == nil || priorFX == nil || currentFX == nil {
		return fx
	}
	for ccy, amt := range exposure {
		if amt == nil || amt.Sign() == 0 {
			continue
		}
		pr, ok1 := priorFX.Rate(ccy)
		cr, ok2 := currentFX.Rate(ccy)
		if !ok1 || !ok2 {
			continue
		}
		delta := new(big.Rat).Sub(cr, pr)
		fx.Add(fx, new(big.Rat).Mul(amt, delta))
	}
	return fx
}
