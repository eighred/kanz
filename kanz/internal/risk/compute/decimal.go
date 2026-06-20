// Package compute implements the risk engine's computation layer —
// exposure (RISK-06), VaR + sensitivities (RISK-07), uncertainty
// propagation (RISK-08). Each computation takes a *domain.Portfolio
// (read under the per-portfolio lock held by the caller, or a
// caller-owned clone) and produces a domain value (ExposureSet,
// MeasureSet) the api/v1 surface can return.
//
// Decimal arithmetic helpers live here because every risk
// computation needs them and proto's common.v1.Decimal is
// representation-only (no methods). Going through these helpers
// (rather than scattering coefficient/exponent math across the
// compute files) keeps a single point of truth for the alignment
// + sign rules.
package compute

import (
	"math"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
)

// zeroDecimal returns a Decimal whose value is exact 0.
func zeroDecimal() *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: 0, Exponent: 0}
}

// addDecimal returns a+b, aligning exponents to the more precise of
// the two. nil operands are treated as 0 — convenient for
// initializing accumulators with a single nil sentinel.
func addDecimal(a, b *commonpb.Decimal) *commonpb.Decimal {
	if a == nil {
		a = zeroDecimal()
	}
	if b == nil {
		b = zeroDecimal()
	}
	targetExp := a.Exponent
	if b.Exponent < targetExp {
		targetExp = b.Exponent
	}
	coefA := a.Coefficient * pow10(int32(a.Exponent-targetExp))
	coefB := b.Coefficient * pow10(int32(b.Exponent-targetExp))
	return &commonpb.Decimal{
		Coefficient: coefA + coefB,
		Exponent:    targetExp,
	}
}

// negateDecimal returns -a. nil → 0.
func negateDecimal(a *commonpb.Decimal) *commonpb.Decimal {
	if a == nil {
		return zeroDecimal()
	}
	return &commonpb.Decimal{Coefficient: -a.Coefficient, Exponent: a.Exponent}
}

// absDecimal returns |a|. nil → 0.
func absDecimal(a *commonpb.Decimal) *commonpb.Decimal {
	if a == nil {
		return zeroDecimal()
	}
	if a.Coefficient < 0 {
		return negateDecimal(a)
	}
	return &commonpb.Decimal{Coefficient: a.Coefficient, Exponent: a.Exponent}
}

// pow10 returns 10^n for small non-negative n. Used for exponent
// alignment in addDecimal. n is bounded by the difference between
// two Decimal exponents — financial data rarely exceeds 8 fractional
// digits, so an overflow guard would be ceremonial; if it ever
// matters (RISK-07 / RISK-08 on extreme scales) switch the helpers
// to math/big.
func pow10(n int32) int64 {
	if n <= 0 {
		return 1
	}
	out := int64(1)
	for i := int32(0); i < n; i++ {
		out *= 10
	}
	return out
}

// mulDecimal returns a*b. coefficient = a.coef × b.coef; exponent =
// a.exp + b.exp. nil → 0. Overflow on the coefficient product is
// possible at extreme scales — see pow10's comment on the math/big
// upgrade path; for the RISK-07 placeholder formulas the int64
// product comfortably holds typical portfolio scales.
func mulDecimal(a, b *commonpb.Decimal) *commonpb.Decimal {
	if a == nil || b == nil {
		return zeroDecimal()
	}
	return &commonpb.Decimal{
		Coefficient: a.Coefficient * b.Coefficient,
		Exponent:    a.Exponent + b.Exponent,
	}
}

// decimalToFloat converts d to float64 for operations (sqrt, log, ...)
// that have no clean integer-arithmetic counterpart. Loses precision
// past ~15 significant digits — fine for RISK-08's uncertainty
// bands (confidence intervals don't need money-grade precision);
// not appropriate for money values, which stay in Decimal end-to-
// end.
func decimalToFloat(d *commonpb.Decimal) float64 {
	if d == nil {
		return 0
	}
	return float64(d.Coefficient) * math.Pow10(int(d.Exponent))
}

// floatToDecimal converts f to a Decimal at the given exponent. The
// coefficient is rounded (not truncated) so the round-trip
// float→decimal→float minimises bias.
func floatToDecimal(f float64, exponent int32) *commonpb.Decimal {
	scaled := f * math.Pow10(int(-exponent))
	return &commonpb.Decimal{
		Coefficient: int64(math.Round(scaled)),
		Exponent:    exponent,
	}
}

// decimalSqrt returns √d via float64 round-trip at uncertaintyExp
// precision. Negative input clamps to zero (sqrt of variance — a
// real variance is non-negative; negative input means upstream
// arithmetic underflowed, which is recovered to "no uncertainty"
// rather than NaN). Output exponent is fixed so propagation chains
// of sqrt-then-sum don't drift toward extreme exponents.
func decimalSqrt(d *commonpb.Decimal) *commonpb.Decimal {
	if d == nil {
		return zeroDecimal()
	}
	f := decimalToFloat(d)
	if f <= 0 {
		return zeroDecimal()
	}
	return floatToDecimal(math.Sqrt(f), uncertaintyExp)
}

// uncertaintyExp is the canonical Decimal exponent for propagated
// uncertainty values: 6 decimal places. Picked to be precise enough
// for typical confidence-interval reporting and coarse enough that
// float64 round-trip cannot meaningfully drift the value.
const uncertaintyExp int32 = -6

// zeroMoney returns a Money worth 0 in the given currency.
func zeroMoney(currency string) *commonpb.Money {
	return &commonpb.Money{Amount: zeroDecimal(), CurrencyCode: currency}
}

// absMoney returns |m| in the same currency.
func absMoney(m *commonpb.Money) *commonpb.Money {
	if m == nil {
		return nil
	}
	return &commonpb.Money{
		Amount:       absDecimal(m.Amount),
		CurrencyCode: m.CurrencyCode,
	}
}

// ShockMoney returns m × (1 + pct) in m's currency. Used by the
// scenario engine (RISK-09) to apply a percentage shock to a
// position's MarketValue without mutating the original. nil m or
// pct returns m unchanged.
//
// Exported (vs the package-private addDecimal/mulDecimal) because
// the scenario package is a sibling under kanz/internal/risk and
// needs the shock primitive at the type boundary; keeping the
// detailed arithmetic helpers package-private means callers only
// see the high-level operation.
func ShockMoney(m *commonpb.Money, pct *commonpb.Decimal) *commonpb.Money {
	if m == nil || pct == nil {
		return m
	}
	one := &commonpb.Decimal{Coefficient: 1, Exponent: 0}
	factor := addDecimal(one, pct)
	return &commonpb.Money{
		Amount:       mulDecimal(m.Amount, factor),
		CurrencyCode: m.CurrencyCode,
	}
}

// addMoney returns a+b, requiring matching currency codes. Cross-
// currency aggregation needs an FX-conversion layer outside this
// package; callers MUST ensure currencies match (the exposure
// computation does so by bucketing on currency first).
func addMoney(a, b *commonpb.Money) *commonpb.Money {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return &commonpb.Money{
		Amount:       addDecimal(a.Amount, b.Amount),
		CurrencyCode: a.CurrencyCode,
	}
}

// AddMoney / AbsMoney / ZeroMoney export the canonical money arithmetic for
// sibling packages that bucket exposures (MODEL-01f factor.SectorExposure)
// without forking the exponent-alignment + sign rules — keeping the single
// point of truth this package's doc insists on. Same same-currency contract as
// addMoney: callers ensure currencies match (factor bucketing restricts to the
// base currency, like the RISK-07 measures).
func AddMoney(a, b *commonpb.Money) *commonpb.Money { return addMoney(a, b) }
func AbsMoney(m *commonpb.Money) *commonpb.Money    { return absMoney(m) }
func ZeroMoney(currency string) *commonpb.Money     { return zeroMoney(currency) }
