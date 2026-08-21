package dec

import (
	"math"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// Float64 converts a Decimal to the nearest float64, reporting whether the
// value was convertible at all.
//
// # Why this exists, and why it is exact
//
// There were EIGHTEEN implementations of this one concept in this module, in
// SEVEN mutually incompatible variants (#628). They disagreed about all three
// properties that matter: what nil means, whether NaN/Inf is refused, and how a
// negative exponent is applied. Fifteen of them computed
//
//	float64(coeff) * math.Pow10(exp)
//
// which ROUNDS TWICE for a negative exponent, because Pow10(-6) is not exactly
// 1e-6. #514 found and documented that; its fix reached three sites and nothing
// else. The fifteen that still carried it included every risk measure, every
// VaR/ES computation, the scenario revaluation shock factor and the performance
// layer's portfolio mark:
//
//	coeff=100  exp=-6    A: 9.999999999999999e-05     exact: 0.0001
//	coeff=123456789 exp=-8  A: 1.2345678900000001    exact: 1.23456789
//
// # Why not #514's fix, which is already in the tree
//
// Because it breaks again below 1e-22. math.Pow10 is only exactly
// representable to 10^22; 10^23 is 1.0000000000000001e+23. So
// `coeff / Pow10(-exp)` double-rounds for exponent <= -23:
//
//	coeff=7 exp=-25   divide: 6.9999999999999995e-25   exact: 7e-25
//
// That fix is correct for the range it was written against and silently wrong
// outside it. Adopting it as THE bridge would bake an unstated bound into the
// platform's only Decimal-to-float conversion.
//
// # Why the exact path costs nothing
//
// FromProto already builds an exact *big.Rat — SetFrac(coeff, 10^-exp) for a
// negative exponent — and FromProtoChecked already gates on InDomain. So this is
// a Float64() call on a value the canonical path has already computed: no new
// arithmetic, and big.Rat.Float64 is correctly rounded ONCE, for every exponent,
// with no representable-range bound. The most correct option is also the one
// that reuses what exists, which is rare enough to be worth stating.
//
// # The bool is not decoration
//
// ok is false for a nil Decimal, for one outside InDomain, and for a magnitude
// that overflows float64. Returning a bare 0 for those is the "confident zero"
// this repository has filed four separate issues about (#617, #618, #621, #623),
// and it was the behaviour at ten of the eighteen sites — including
// risk/compute's own bridge, for the measure engine itself. A caller that
// genuinely wants a fallback says so with Float64Or.
// maxExactCoefficient is the largest integer every value below which is exactly
// representable as a float64 (2^53). Above it float64(coeff) already rounds, so
// the "one rounding" argument no longer holds and the Rat path takes over.
const maxExactCoefficient = 1 << 53

// maxExactPow10 is the largest k for which 10^k is exactly representable as a
// float64. 10^23 is 1.0000000000000001e+23 — which is why #514's own fix,
// `coeff / Pow10(-exp)`, double-rounds again from exponent -23 downward.
const maxExactPow10 = 22

// pow10Exact holds 10^0..10^22 as float64 literals, every one of them exact.
// Written out rather than computed with math.Pow10 BECAUSE math.Pow10 is the
// thing being avoided: a table built by calling it would inherit its rounding.
var pow10Exact = [maxExactPow10 + 1]float64{
	1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10, 1e11,
	1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18, 1e19, 1e20, 1e21, 1e22,
}

func Float64(d *commonpb.Decimal) (float64, bool) {
	// NIL IS CHECKED HERE AND NOT LEFT TO FromProtoChecked, because InDomain
	// returns TRUE for nil — deliberately, since it answers a question about the
	// EXPONENT RANGE and a nil decimal has no exponent to be out of range. That
	// is right for its callers and wrong for this one: the question here is
	// whether there is a value to convert at all, and a nil that arrived as 0
	// with ok=true would be precisely the confident zero this bridge exists to
	// end. Caught by TestFloat64RefusesWhatItCannotConvert while writing it.
	if d == nil {
		return 0, false
	}
	if !InDomain(d) {
		return 0, false
	}

	// THE ALLOCATION-FREE PATH, WHICH IS NOT AN APPROXIMATION OF THE EXACT ONE.
	//
	// The first cut of this bridge went straight to big.Rat and was correct — and
	// TestComputeMeasures_AllocsConstantInN caught it immediately: ComputeMeasures
	// went from O(1) allocations to 8588 per call at n=1024, because a Rat is
	// built PER POSITION inside the measure loop. That guard exists to keep the
	// measures query's allocation count independent of portfolio size, and it was
	// right to fire.
	//
	// WHY THIS IS EXACT RATHER THAN MERELY CLOSE. IEEE-754 requires every
	// arithmetic operation to be correctly rounded, so ONE multiply or divide of
	// two EXACTLY-REPRESENTABLE operands rounds exactly once — which is the same
	// value big.Rat.Float64 produces. The whole defect #514 documented was the
	// SECOND rounding: Pow10(-6) is not exactly 1e-6, so multiplying by it rounds
	// while building the power and again in the product. Both preconditions are
	// enforced here rather than assumed:
	//
	//   - |coefficient| <= 2^53, so float64(coeff) is exact
	//   - the power of ten is <= 10^22, the largest exactly representable one
	//
	// Outside either, control falls through to the Rat path, which has no range
	// bound. That is the ordering that makes this safe: the fast path is a
	// narrowing, never a fallback, and TestFastAndExactPathsAgree checks the two
	// against each other across the whole precondition rather than trusting this
	// paragraph.
	coeff, exp := d.GetCoefficient(), int(d.GetExponent())
	if coeff <= maxExactCoefficient && coeff >= -maxExactCoefficient {
		switch {
		case exp >= 0 && exp <= maxExactPow10:
			f := float64(coeff) * pow10Exact[exp]
			if !math.IsInf(f, 0) && !math.IsNaN(f) {
				return f, true
			}
		case exp < 0 && -exp <= maxExactPow10:
			return float64(coeff) / pow10Exact[-exp], true
		}
	}

	r, ok := FromProtoChecked(d)
	if !ok {
		return 0, false
	}
	f, _ := r.Float64()
	// big.Rat.Float64 returns ±Inf for a magnitude beyond float64. A risk
	// measure computed from +Inf is not a large number, it is a number that
	// poisons every aggregate it enters — and NaN compares false against every
	// threshold, so a limit check on one silently passes.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// Float64Or converts a Decimal to the nearest float64, returning fallback when
// it is not convertible.
//
// IT EXISTS SO THE FALLBACK IS VISIBLE. Ten of the eighteen implementations
// #628 retired returned a bare 0 for nil, which is indistinguishable at the call
// site from a decimal that genuinely was zero. Those call sites now read
// `dec.Float64Or(x, 0)`, which is the same behaviour and says so — greppable,
// reviewable, and countable, which is what the confident-zero issues (#617,
// #618, #621, #623) need in order to be worked at all.
//
// It is NOT an endorsement of that fallback. A caller that can distinguish
// "absent" from "zero" should use Float64 and act on ok; this is for the ones
// that cannot yet, so the arithmetic fix could land without also changing risk
// outputs in the same commit.
func Float64Or(d *commonpb.Decimal, fallback float64) float64 {
	if f, ok := Float64(d); ok {
		return f
	}
	return fallback
}
