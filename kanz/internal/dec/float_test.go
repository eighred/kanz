package dec

import (
	"math"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

// fdec builds a Decimal for these cases. Named fdec rather than d because this
// package's other tests already declare a d helper.
func fdec(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

// THE ARITHMETIC #514 FOUND, AND THE RANGE ITS FIX DOES NOT COVER (#628).
//
// Each want below was computed independently, outside Go, from an exact
// rational — `python3 -c "from fractions import Fraction; float(Fraction(c,
// 10**-e))"` — rather than by running the code under test and recording what it
// said. A test whose expected values come from the implementation proves the
// implementation is consistent with itself.
func TestFloat64IsCorrectlyRoundedOnce(t *testing.T) {
	cases := []struct {
		name  string
		coeff int64
		exp   int32
		want  float64
	}{
		// The two the fifteen `coeff * Pow10(exp)` sites get WRONG. Pow10(-6) is
		// not exactly 1e-6, so that form rounds twice.
		{"the #514 case", 100, -6, 0.0001},
		{"eight decimal places", 123456789, -8, 1.23456789},

		// The range #514's own fix does not reach: Pow10 is exact only to 10^22,
		// so `coeff / Pow10(-exp)` double-rounds from -23 down.
		{"below the representable power", 7, -25, 7e-25},

		// Ordinary values every variant agrees on — present so a bridge that
		// refused everything could not pass on the interesting rows alone.
		{"positive exponent", 5, 2, 500},
		{"one tenth", 1, -1, 0.1},
		{"zero", 0, -6, 0},
		{"negative", -25, -2, -0.25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Float64(fdec(tc.coeff, tc.exp))
			if !ok {
				t.Fatalf("Float64(%d, %d) refused a convertible value", tc.coeff, tc.exp)
			}
			if got != tc.want {
				t.Errorf("Float64(%d, %d) = %v, want %v (%d ULP away).\n\n"+
					"This is the double-rounding #514 documented: `coeff * Pow10(exp)` for a negative "+
					"exponent rounds once building the power and again multiplying. The bridge converts "+
					"the exact big.Rat instead, which rounds once.",
					tc.coeff, tc.exp, got, tc.want, ulpsApart(got, tc.want))
			}
		})
	}
}

// THE THREE REFUSALS, EACH OF WHICH WAS A SILENT ZERO SOMEWHERE.
func TestFloat64RefusesWhatItCannotConvert(t *testing.T) {
	t.Run("nil is not zero", func(t *testing.T) {
		got, ok := Float64(nil)
		if ok {
			t.Fatalf("Float64(nil) = %v, true — an absent decimal reported as a value is the "+
				"confident zero this repository has four open issues about", got)
		}
	})

	// A magnitude past float64. big.Rat.Float64 returns ±Inf, and an Inf that
	// reaches a risk measure does not read as "large" — it poisons every
	// aggregate it enters, and a NaN derived from it compares false against
	// every threshold, so a limit check on one silently passes.
	t.Run("overflow is refused, not returned as Inf", func(t *testing.T) {
		huge := fdec(math.MaxInt64, 300)
		got, ok := Float64(huge)
		if ok && (math.IsInf(got, 0) || math.IsNaN(got)) {
			t.Fatalf("Float64 returned %v with ok=true — an Inf or NaN handed to a measure is worse "+
				"than a refusal, because nothing downstream checks for it", got)
		}
		if ok {
			t.Fatalf("Float64 accepted a value beyond float64: %v", got)
		}
	})

	t.Run("Float64Or substitutes only when the conversion failed", func(t *testing.T) {
		if got := Float64Or(nil, -1); got != -1 {
			t.Errorf("Float64Or(nil, -1) = %v, want -1", got)
		}
		if got := Float64Or(fdec(100, -6), -1); got != 0.0001 {
			t.Errorf("Float64Or(0.0001, -1) = %v, want 0.0001 — the fallback must not shadow a "+
				"value that converted cleanly", got)
		}
	})
}

// ulpsApart reports how many representable float64s separate a and b, so a
// failure says "one ULP" rather than printing two numbers that look identical.
func ulpsApart(a, b float64) int64 {
	ia := int64(math.Float64bits(a))
	ib := int64(math.Float64bits(b))
	if ia < 0 {
		ia = math.MinInt64 - ia
	}
	if ib < 0 {
		ib = math.MinInt64 - ib
	}
	if ia > ib {
		return ia - ib
	}
	return ib - ia
}

// THE FAST PATH IS A NARROWING OF THE EXACT ONE, NOT A SECOND ANSWER (#628).
//
// Float64 has two branches: an allocation-free IEEE one inside a stated
// precondition, and big.Rat outside it. That is exactly the shape this issue
// exists to end — one concept, two implementations — so the equivalence is
// EXECUTED across the whole precondition rather than argued in the doc comment.
//
// The reference here is FromProto().Float64(), the exact path, computed
// independently of whatever the fast branch did.
func TestFastAndExactPathsAgree(t *testing.T) {
	coeffs := []int64{
		0, 1, -1, 7, -7, 100, 999, 1000, 123456789, -123456789,
		1 << 20, 1 << 40, 1 << 52, (1 << 53) - 1, 1 << 53, -(1 << 53),
		math.MaxInt64, math.MinInt64 + 1,
	}
	checked := 0
	for _, c := range coeffs {
		for exp := -30; exp <= 30; exp++ {
			d := fdec(c, int32(exp))
			got, ok := Float64(d)
			if !ok {
				continue // refusals are TestFloat64RefusesWhatItCannotConvert's business
			}
			want, _ := FromProto(d).Float64()
			if math.IsInf(want, 0) || math.IsNaN(want) {
				continue
			}
			checked++
			if got != want {
				t.Fatalf("Float64(coeff=%d, exp=%d) = %v, but the exact big.Rat path gives %v "+
					"(%d ULP apart).\n\n"+
					"The fast branch has stopped being a narrowing of the exact one, which is the "+
					"two-implementations-of-one-concept defect #628 was filed for — reappearing INSIDE "+
					"the helper that retired the other eighteen.",
					c, exp, got, want, ulpsApart(got, want))
			}
		}
	}
	// NON-VACUITY: a Float64 that refused everything would satisfy the loop above
	// without comparing anything.
	if checked < 500 {
		t.Fatalf("only %d (coeff, exp) pairs actually converted — the sweep is not exercising the "+
			"fast path and this test proves nothing", checked)
	}
	t.Logf("fast and exact paths agree on %d (coefficient, exponent) pairs", checked)
}
