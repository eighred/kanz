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

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	// decutil: this package's own tests declare a local `dec(...)` Decimal-literal
	// helper (decimal_test.go), so the platform decimal package is aliased here
	// to avoid the name clash — the same alias internal/compliance uses.
	decutil "github.com/eighred/kanz/internal/dec"
)

// THE ARITHMETIC BELOW IS decutil.Add / decutil.Mul / decutil.Abs, NOT A LOCAL COPY (#216).
//
// It used to be a local copy, and the copy was the bug. The identical int64
// coefficient arithmetic was written three times — the compliance gate, the
// platform dec package, and here — and repaired twice. A -20% stress on a
// $200,000 position returned -$24,467 instead of +$160,000: 2e13 × 8e5 = 1.6e19
// clears MaxInt64 and wraps NEGATIVE, so the scenario engine reported that a 20%
// crash makes the desk money. The wrap point for a -20% shock is a position of
// $115,292.15.
//
// It survived two repairs because a copied helper does not receive fixes; it
// receives them only where somebody remembers to look. The wrappers here are
// thin on purpose — enough to name the operation in the compute layer's
// vocabulary and to state what a refusal means, and no arithmetic of their own.
//
// A REFUSAL SURFACES AS nil, never as a number. decutil.Add/decutil.Mul refuse only when
// the exponent itself cannot move, which for in-domain inputs (|exponent| ≤ 64,
// enforced at every ingress — decutil.InDomainDeep on the bus, decutil.InDomainDeep on
// the gRPC query surface since #246) cannot happen — internal/dec's
// TestArithmeticIsTotalOnInDomainInputs pins exactly that, by sweeping every
// extreme in-domain exponent/coefficient pair and asserting no refusal is
// possible. Reaching nil therefore means an ingress guard was
// removed, and the honest answer is an ABSENT measure, not a wrapped one and
// never a zero — a zero exposure reads as flat and passes every limit check.

// zeroDecimal returns a Decimal whose value is exact 0.
func zeroDecimal() *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: 0, Exponent: 0}
}

// addDecimal returns a+b, aligning exponents to the more precise of
// the two. nil operands are treated as 0 — convenient for
// initializing accumulators with a single nil sentinel. nil result ⇒
// unrepresentable; see the file header.
func addDecimal(a, b *commonpb.Decimal) *commonpb.Decimal {
	out, ok := decutil.Add(a, b)
	if !ok {
		return nil
	}
	return out
}

// negateDecimal returns -a. nil → 0.
//
// MinInt64 is the case that is not arithmetic-as-usual: Go's -MinInt64 is
// MinInt64, so the naive negation returns the SAME negative number. Through
// absDecimal that means |x| comes back negative and a gross exposure SUBTRACTS
// the position it should have enlarged. decutil.Abs raises the exponent instead.
func negateDecimal(a *commonpb.Decimal) *commonpb.Decimal {
	if a == nil {
		return zeroDecimal()
	}
	if a.GetCoefficient() != math.MinInt64 {
		return &commonpb.Decimal{Coefficient: -a.Coefficient, Exponent: a.Exponent}
	}
	out, ok := decutil.Abs(a)
	if !ok {
		return nil
	}
	return out
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

// maxPow10 is the largest n for which 10^n is an int64: 10^18 fits,
// 10^19 is above MaxInt64.
const maxPow10 = 18

// pow10 returns 10^n for non-negative n, and whether it fits an int64.
//
// It is BOUNDED, and the bound is the point (#246). This was an unbounded
// `for i := 0; i < n; i++` loop justified by a comment saying "n is bounded by
// the difference between two Decimal exponents … an overflow guard would be
// ceremonial". Nothing bounded that difference: Decimal.exponent is a wire
// field, and the risk engine's gRPC query surface passed a caller-supplied shock
// Pct straight through with no domain check. exponent -2000000000 meant two
// billion iterations PER POSITION, PER SHOCK — the engine stops answering while
// still reporting healthy, which is the worst shape of failure this platform
// has. It also produced a silently wrong scale factor for any n ≥ 19.
//
// Refusing past 10^18 costs a correct caller nothing: there is no int64 to
// return there. decAccum's fast path reads false as "spill to the exact
// math/big path", so the bound degrades precision-of-representation, never the
// answer.
func pow10(n int64) (int64, bool) {
	if n <= 0 {
		return 1, true
	}
	if n > maxPow10 {
		return 0, false
	}
	out := int64(1)
	for i := int64(0); i < n; i++ {
		out *= 10
	}
	return out, true
}

// mulInt64 returns x*y and whether it stayed inside int64.
func mulInt64(x, y int64) (int64, bool) {
	if x == 0 || y == 0 {
		return 0, true
	}
	if x == math.MinInt64 || y == math.MinInt64 {
		return 0, false // the division check below cannot see this one
	}
	z := x * y
	if z/y != x {
		return 0, false
	}
	return z, true
}

// addInt64 returns x+y and whether it stayed inside int64.
func addInt64(x, y int64) (int64, bool) {
	z := x + y
	if (y > 0 && z < x) || (y < 0 && z > x) {
		return 0, false
	}
	return z, true
}

// mulDecimal returns a*b. nil → 0. nil result ⇒ unrepresentable; see the file
// header. This is the function #216 was filed against — it multiplied raw int64
// coefficients and wrapped, sign and all.
func mulDecimal(a, b *commonpb.Decimal) *commonpb.Decimal {
	out, ok := decutil.Mul(a, b)
	if !ok {
		return nil
	}
	return out
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
	f := decutil.Float64Or(d, 0)
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

// decAccum is an allocation-free running Decimal sum. Folding a slice with
// addDecimal heap-allocates a fresh *Decimal per term (LATENCY-01b: ~75% of a
// measures query's allocations were these per-position sums); decAccum keeps the
// running value in plain coefficient/exponent fields and materializes a single
// *Decimal at the end.
//
// Its zero value is exact 0 at exponent 0 — identical to the zeroDecimal() seed
// the old addDecimal fold started from — so a sum built with decAccum is
// byte-identical (coefficient AND exponent) to the same sum built with
// addDecimal, not merely numerically equal.
//
// THAT IDENTITY IS WHY IT SPILLS (#216). The int64 fast path is kept — it is
// what the LATENCY-01c allocation guard measures, and every realistic book
// stays on it — but it now DETECTS the overflow it used to commit and hands the
// running sum to decutil.Add from there. Detection matters most where no multiply
// is involved at all: after a scenario shock the shocked positions carry a
// coarser exponent than the untouched ones, so aligning an unshocked $10,000,000
// (1e15 at -8) to a shocked -13 multiplies it by 1e5 and leaves int64 range on
// its own. Gross exposure is what a limit is checked against, so a wrapped sum
// there is a limit that passes on a book that breached it.
type decAccum struct {
	coef int64
	exp  int32
	// spilled is the running sum once the int64 fast path could no longer hold
	// it. nil while the fast path holds, which is the allocation-free case.
	spilled *commonpb.Decimal
	// refused latches a sum decutil.Add could not represent at all. The accumulator
	// then yields nil forever: a partial sum is a WRONG total, not a smaller one.
	refused bool
}

// add folds d into the accumulator, aligning exponents to the more precise
// (smaller) of the two exactly as addDecimal does. nil ⇒ 0 (no-op). When abs is
// true the term's magnitude is taken first (the gross-exposure case), matching
// addDecimal(sum, absDecimal(d)).
func (a *decAccum) add(d *commonpb.Decimal, abs bool) {
	if d == nil || a.refused {
		return
	}
	c, e := d.GetCoefficient(), int64(d.GetExponent())
	// |MinInt64| has no int64 coefficient at this exponent, so the fast path
	// cannot take this magnitude — negating it would leave the term NEGATIVE in
	// a gross sum. negateDecimal raises the exponent instead.
	viaExact := abs && c == math.MinInt64
	if abs && c < 0 && !viaExact {
		c = -c
	}
	if !viaExact && a.spilled == nil && a.addFast(c, e) {
		return
	}

	term := &commonpb.Decimal{Coefficient: c, Exponent: int32(e)}
	if viaExact {
		if term = negateDecimal(term); term == nil {
			a.refused = true
			return
		}
	}
	if a.spilled == nil {
		a.spilled = &commonpb.Decimal{Coefficient: a.coef, Exponent: a.exp}
	}
	sum, ok := decutil.Add(a.spilled, term)
	if !ok {
		a.refused, a.spilled = true, nil
		return
	}
	a.spilled = sum
}

// addFast folds (c, e) into the int64 accumulator, applying exactly the
// alignment rule decutil.Add applies. It reports false — WITHOUT having mutated the
// accumulator — when any step would leave int64 range; that is the spill
// signal, and it is the difference between a coarser answer and a wrong one.
//
// Allocation-free by construction: no *Decimal is built and nothing reaches
// math/big. TestComputeMeasures_AllocsConstantInN pins that this stays the path
// an ordinary book takes.
func (a *decAccum) addFast(c, e int64) bool {
	// A zero operand has no magnitude, so its exponent must not drag the
	// alignment — the same rule decutil.Add applies, and what keeps this
	// accumulator byte-identical to an addDecimal fold rather than merely equal
	// in value.
	if c == 0 {
		return true
	}
	if a.coef == 0 {
		a.coef, a.exp = c, int32(e)
		return true
	}
	gap := int64(a.exp) - e // both operands came from int32; cannot overflow
	if gap > 0 {            // the incoming term is more precise: rescale the accumulator
		p, ok := pow10(gap)
		if !ok {
			return false
		}
		scaled, ok := mulInt64(a.coef, p)
		if !ok {
			return false
		}
		sum, ok := addInt64(scaled, c)
		if !ok {
			return false
		}
		a.coef, a.exp = sum, int32(e)
		return true
	}
	p, ok := pow10(-gap)
	if !ok {
		return false
	}
	scaled, ok := mulInt64(c, p)
	if !ok {
		return false
	}
	sum, ok := addInt64(a.coef, scaled)
	if !ok {
		return false
	}
	a.coef = sum
	return true
}

// decimal materializes the accumulated value as a fresh *Decimal, or nil when
// the sum was refused (see decAccum.refused).
func (a *decAccum) decimal() *commonpb.Decimal {
	if a.refused {
		return nil
	}
	if a.spilled != nil {
		return &commonpb.Decimal{Coefficient: a.spilled.GetCoefficient(), Exponent: a.spilled.GetExponent()}
	}
	return &commonpb.Decimal{Coefficient: a.coef, Exponent: a.exp}
}

// money materializes the accumulated value as Money in the given currency. A
// refused sum yields a Money with a nil Amount — absent, which every reader of
// a Money already handles, and which cannot be mistaken for a flat book.
func (a *decAccum) money(currency string) *commonpb.Money {
	return &commonpb.Money{Amount: a.decimal(), CurrencyCode: currency}
}

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
