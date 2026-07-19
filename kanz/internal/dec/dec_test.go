package dec

import (
	"math/big"
	"testing"
)

// TestToProtoExact_RepresentableValueRoundTrips pins that a value whose scaled
// coefficient fits in an int64 reports ok and produces the expected Decimal.
func TestToProtoExact_RepresentableValueRoundTrips(t *testing.T) {
	r := big.NewRat(100, 1) // 100.00000000 at scale 8
	got, ok := ToProtoExact(r)
	if !ok {
		t.Fatalf("ok = false, want true for a representable value")
	}
	want := int64(100 * 1e8)
	if got.GetCoefficient() != want || got.GetExponent() != -scale {
		t.Fatalf("got = {coeff:%d exp:%d}, want {coeff:%d exp:%d}",
			got.GetCoefficient(), got.GetExponent(), want, -scale)
	}
}

// TestToProtoExact_OverflowingValueReportsNotOK pins the exact case this
// function exists for: 184467440738 scaled by 10^8 lands just past the int64
// range and would silently wrap through big.Int.Int64() if unchecked.
func TestToProtoExact_OverflowingValueReportsNotOK(t *testing.T) {
	r := big.NewRat(184467440738, 1)
	got, ok := ToProtoExact(r)
	if ok {
		t.Fatalf("ok = true, want false — %s scaled by 10^%d overflows int64", r.String(), scale)
	}
	if got != nil {
		t.Fatalf("got = %+v, want nil when not representable", got)
	}
}

// TestToProtoExact_AgreesWithToProto is the test that makes this refactor
// worth doing: for every representable input, ToProtoExact's coefficient must
// be IDENTICAL to ToProto's. If the two ever compute the scaled/rounded value
// differently, this test catches the drift immediately — the two conversions
// share one arithmetic path, so drift should be structurally impossible, but
// this is the tripwire in case that ever stops being true.
func TestToProtoExact_AgreesWithToProto(t *testing.T) {
	cases := []*big.Rat{
		big.NewRat(0, 1),
		big.NewRat(1, 3),
		big.NewRat(-1, 3),
		big.NewRat(100, 1),
		big.NewRat(1, 100000000),
		big.NewRat(99999999, 1),
		big.NewRat(-99999999, 1),
		Rat("0.123456785"), // exercises half-up rounding
		Rat("0.123456784"),
	}
	for _, r := range cases {
		want := ToProto(r)
		got, ok := ToProtoExact(r)
		if !ok {
			t.Fatalf("ToProtoExact(%s): ok = false, want true", r.String())
		}
		if got.GetCoefficient() != want.GetCoefficient() || got.GetExponent() != want.GetExponent() {
			t.Fatalf("ToProtoExact(%s) = %+v, ToProto(%s) = %+v — must agree exactly",
				r.String(), got, r.String(), want)
		}
	}
}

// TestToProto_LiteralOutput pins ToProto's ABSOLUTE output.
//
// TestToProtoExact_AgreesWithToProto above cannot fail while both functions
// delegate to scaledCoefficient, and it says nothing about the overflow region
// where the two are DESIGNED to differ. The real regression risk is that
// scaledCoefficient changes and silently alters ToProto for every caller in
// the repository. This table is what makes that impossible: it names the
// expected coefficient and exponent outright, including rounding ties in both
// signs, values against the int64 boundary, and — deliberately — one
// overflowing value pinning that ToProto STILL WRAPS exactly as it always has.
// That wrapping is preserved on purpose for existing callers; ToProtoExact is
// the variant a capital path must use.
func TestToProto_LiteralOutput(t *testing.T) {
	cases := []struct {
		name      string
		in        *big.Rat
		wantCoeff int64
		wantExp   int32
		wantOK    bool // what ToProtoExact must report for the same input
	}{
		{"zero", big.NewRat(0, 1), 0, -8, true},
		{"one", big.NewRat(1, 1), 100000000, -8, true},
		{"smallest representable", big.NewRat(1, 100000000), 1, -8, true},
		{"negative smallest", big.NewRat(-1, 100000000), -1, -8, true},
		{"third truncates and rounds up", big.NewRat(1, 3), 33333333, -8, true},
		{"negative third", big.NewRat(-1, 3), -33333333, -8, true},
		{"two thirds rounds up", big.NewRat(2, 3), 66666667, -8, true},
		{"negative two thirds rounds away from zero", big.NewRat(-2, 3), -66666667, -8, true},

		// Rounding TIES, both signs: half-up is away from zero.
		{"positive tie rounds up", Rat("0.000000005"), 1, -8, true},
		{"negative tie rounds down", Rat("-0.000000005"), -1, -8, true},
		{"positive tie mid-magnitude", Rat("1.234567895"), 123456790, -8, true},
		{"negative tie mid-magnitude", Rat("-1.234567895"), -123456790, -8, true},
		{"just below a tie", Rat("0.123456784"), 12345678, -8, true},
		{"just above a tie", Rat("0.123456786"), 12345679, -8, true},

		// The int64 boundary. 92233720368.54775807 × 10^8 is exactly MaxInt64.
		{"exactly MaxInt64 scaled", Rat("92233720368.54775807"), 9223372036854775807, -8, true},
		{"exactly MinInt64 scaled", Rat("-92233720368.54775808"), -9223372036854775808, -8, true},

		// OVERFLOW: ToProto wraps (big.Int.Int64()), by design, for existing
		// callers. This literal is the tripwire on that documented behaviour.
		{"overflow wraps", big.NewRat(184467440738, 1), 90448384, -8, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ToProto(tc.in)
			if got.GetCoefficient() != tc.wantCoeff || got.GetExponent() != tc.wantExp {
				t.Fatalf("ToProto(%s) = {coeff:%d exp:%d}, want {coeff:%d exp:%d}",
					tc.in.String(), got.GetCoefficient(), got.GetExponent(), tc.wantCoeff, tc.wantExp)
			}
			exact, ok := ToProtoExact(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ToProtoExact(%s): ok = %v, want %v", tc.in.String(), ok, tc.wantOK)
			}
			if !ok {
				if exact != nil {
					t.Fatalf("ToProtoExact(%s) = %+v, want nil when not representable", tc.in.String(), exact)
				}
				return
			}
			if exact.GetCoefficient() != tc.wantCoeff || exact.GetExponent() != tc.wantExp {
				t.Fatalf("ToProtoExact(%s) = {coeff:%d exp:%d}, want {coeff:%d exp:%d}",
					tc.in.String(), exact.GetCoefficient(), exact.GetExponent(), tc.wantCoeff, tc.wantExp)
			}
		})
	}
}

// TestToProtoExact_OKBoundaryIsPrecise pins where ok flips: one ulp inside the
// int64 range must be representable, one ulp outside must not.
func TestToProtoExact_OKBoundaryIsPrecise(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
	}{
		{"92233720368.54775807", true},   // MaxInt64 / 10^8
		{"92233720368.54775808", false},  // one ulp past
		{"-92233720368.54775808", true},  // MinInt64 / 10^8
		{"-92233720368.54775809", false}, // one ulp past
	}
	for _, tc := range cases {
		_, ok := ToProtoExact(Rat(tc.in))
		if ok != tc.wantOK {
			t.Fatalf("ToProtoExact(%s): ok = %v, want %v", tc.in, ok, tc.wantOK)
		}
	}
}
