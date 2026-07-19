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
