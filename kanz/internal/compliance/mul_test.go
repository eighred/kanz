package compliance

import (
	"math"
	"math/big"
	"testing"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	decutil "github.com/kanz-eng/kanz/internal/dec"
)

// TestMulDecimal_ExactInTheNormalRange pins that every product which fits an
// int64 at its natural exponent is returned UNCHANGED — same coefficient, same
// exponent as the old raw multiply. The fix must not move a single result in
// the range that never wrapped.
func TestMulDecimal_ExactInTheNormalRange(t *testing.T) {
	cases := []struct {
		name           string
		a, b           *commonpb.Decimal
		wantC          int64
		wantE          int32
		wantNilProduct bool
	}{
		{name: "nil a", a: nil, b: &commonpb.Decimal{Coefficient: 5}, wantNilProduct: true},
		{name: "nil b", a: &commonpb.Decimal{Coefficient: 5}, b: nil, wantNilProduct: true},
		{name: "zero", a: &commonpb.Decimal{}, b: &commonpb.Decimal{Coefficient: 7, Exponent: -2}, wantC: 0, wantE: -2},
		{
			name:  "ten at one hundred",
			a:     &commonpb.Decimal{Coefficient: 10, Exponent: 0},
			b:     &commonpb.Decimal{Coefficient: 100, Exponent: 0},
			wantC: 1000, wantE: 0,
		},
		{
			name:  "mark-style exponent -8",
			a:     &commonpb.Decimal{Coefficient: 10, Exponent: 0},
			b:     &commonpb.Decimal{Coefficient: 100 * 1e8, Exponent: -8},
			wantC: 100000000000, wantE: -8,
		},
		{
			name:  "negative quantity (a sell)",
			a:     &commonpb.Decimal{Coefficient: -10, Exponent: 0},
			b:     &commonpb.Decimal{Coefficient: 250, Exponent: -2},
			wantC: -2500, wantE: -2,
		},
		{
			name:  "both negative",
			a:     &commonpb.Decimal{Coefficient: -10, Exponent: -1},
			b:     &commonpb.Decimal{Coefficient: -250, Exponent: -2},
			wantC: 2500, wantE: -3,
		},
		{
			// Exactly MaxInt64 as a product: the largest value that must NOT be
			// rescaled.
			name:  "product is exactly MaxInt64",
			a:     &commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: 0},
			b:     &commonpb.Decimal{Coefficient: 1, Exponent: 0},
			wantC: math.MaxInt64, wantE: 0,
		},
		{
			name:  "product is exactly MinInt64",
			a:     &commonpb.Decimal{Coefficient: math.MinInt64, Exponent: -3},
			b:     &commonpb.Decimal{Coefficient: 1, Exponent: 2},
			wantC: math.MinInt64, wantE: -1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := mulDecimal(tc.a, tc.b)
			if !ok {
				t.Fatalf("ok = false, want true")
			}
			if tc.wantNilProduct {
				if got.GetCoefficient() != 0 || got.GetExponent() != 0 {
					t.Fatalf("got = %+v, want the zero Decimal for a nil operand", got)
				}
				return
			}
			if got.GetCoefficient() != tc.wantC || got.GetExponent() != tc.wantE {
				t.Fatalf("got = {coeff:%d exp:%d}, want {coeff:%d exp:%d}",
					got.GetCoefficient(), got.GetExponent(), tc.wantC, tc.wantE)
			}
		})
	}
}

// TestMulDecimal_OverflowRescalesAndKeepsMagnitude is the heart of the fix: a
// product past int64 must NOT wrap. It must come back at a coarser exponent
// with its MAGNITUDE intact — which is the only property a concentration rule
// needs. The old code returned a small, often negative, fabricated number here.
func TestMulDecimal_OverflowRescalesAndKeepsMagnitude(t *testing.T) {
	cases := []struct {
		name string
		a, b *commonpb.Decimal
		// want is the true product as an exact decimal string; the returned
		// Decimal must equal it to within one unit of its own exponent.
		want string
	}{
		{
			// THE REPRO: 1,844,674,407 shares × a $100 mark at exponent -8.
			name: "the mark-path repro",
			a:    &commonpb.Decimal{Coefficient: 1844674407, Exponent: 0},
			b:    &commonpb.Decimal{Coefficient: 100 * 1e8, Exponent: -8},
			want: "184467440700",
		},
		{
			name: "9.2e9 shares at a $100 mark",
			a:    &commonpb.Decimal{Coefficient: 9223372036, Exponent: 0},
			b:    &commonpb.Decimal{Coefficient: 100 * 1e8, Exponent: -8},
			want: "922337203600",
		},
		{
			name: "the limit path at an absurd size",
			a:    &commonpb.Decimal{Coefficient: 184467440737095516, Exponent: 0},
			b:    &commonpb.Decimal{Coefficient: 100, Exponent: 0},
			want: "18446744073709551600",
		},
		{
			name: "MaxInt64 squared",
			a:    &commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: 0},
			b:    &commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: 0},
			want: "85070591730234615847396907784232501249",
		},
		{
			name: "MinInt64 squared is positive",
			a:    &commonpb.Decimal{Coefficient: math.MinInt64, Exponent: 0},
			b:    &commonpb.Decimal{Coefficient: math.MinInt64, Exponent: 0},
			want: "85070591730234615865843651857942052864",
		},
		{
			name: "MinInt64 times MaxInt64 stays negative",
			a:    &commonpb.Decimal{Coefficient: math.MinInt64, Exponent: 0},
			b:    &commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: 0},
			want: "-85070591730234615856620279821087277056",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := mulDecimal(tc.a, tc.b)
			if !ok {
				t.Fatalf("ok = false, want true — %s is comfortably representable at a coarser exponent", tc.want)
			}
			want := decutil.Rat(tc.want)
			// Tolerance: one unit at the RETURNED exponent — the precision the
			// rescale deliberately dropped, and nothing more.
			ulp := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(got.GetExponent())), nil))
			if got.GetExponent() < 0 {
				ulp = new(big.Rat).SetFrac(big.NewInt(1),
					new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-got.GetExponent())), nil))
			}
			diff := new(big.Rat).Sub(decutil.FromProto(got), want)
			diff.Abs(diff)
			if diff.Cmp(ulp) > 0 {
				t.Fatalf("mulDecimal = %s (coeff:%d exp:%d), want %s within one ulp (%s) — the magnitude was lost",
					decutil.Str(decutil.FromProto(got)), got.GetCoefficient(), got.GetExponent(), tc.want, decutil.Str(ulp))
			}
			// And the sign must survive, which is what wrapping destroyed.
			if decutil.FromProto(got).Sign() != want.Sign() {
				t.Fatalf("sign flipped: got %s, want %s", decutil.Str(decutil.FromProto(got)), tc.want)
			}
		})
	}
}

// TestMulDecimal_RescaleRoundsHalfAwayFromZero pins the rounding the rescale
// introduces, in both signs — the only rounding this function has.
func TestMulDecimal_RescaleRoundsHalfAwayFromZero(t *testing.T) {
	// 9223372036854775807 × 5 = 46116860184273879035, one digit too wide; the
	// dropped digit is 5, so it rounds away from zero to 4611686018427387904
	// (i.e. ...903.5 → ...904) at exponent +1.
	got, ok := mulDecimal(
		&commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: 0},
		&commonpb.Decimal{Coefficient: 5, Exponent: 0},
	)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got.GetCoefficient() != 4611686018427387904 || got.GetExponent() != 1 {
		t.Fatalf("got = {coeff:%d exp:%d}, want {coeff:4611686018427387904 exp:1}",
			got.GetCoefficient(), got.GetExponent())
	}

	gotNeg, ok := mulDecimal(
		&commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: 0},
		&commonpb.Decimal{Coefficient: -5, Exponent: 0},
	)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if gotNeg.GetCoefficient() != -4611686018427387904 || gotNeg.GetExponent() != 1 {
		t.Fatalf("got = {coeff:%d exp:%d}, want {coeff:-4611686018427387904 exp:1}",
			gotNeg.GetCoefficient(), gotNeg.GetExponent())
	}
}

// TestMulDecimal_RefusesWhenTheExponentCannotBeRepresented pins the one case
// where the product is genuinely unvaluable: the exponent has nowhere left to
// go. The contract is REFUSE — never a wrapped number.
func TestMulDecimal_RefusesWhenTheExponentCannotBeRepresented(t *testing.T) {
	got, ok := mulDecimal(
		&commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: math.MaxInt32},
		&commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: 0},
	)
	if ok {
		t.Fatalf("ok = true, want false — the exponent cannot be raised past MaxInt32 (got %+v)", got)
	}
	if got != nil {
		t.Fatalf("got = %+v, want nil when not representable", got)
	}

	// The mirror case: an exponent sum below MinInt32 can never be repaired by
	// rescaling, which only ever raises the exponent.
	if _, ok := mulDecimal(
		&commonpb.Decimal{Coefficient: 2, Exponent: math.MinInt32},
		&commonpb.Decimal{Coefficient: 2, Exponent: -1},
	); ok {
		t.Fatal("ok = true, want false — the exponent sum is below MinInt32")
	}
}
