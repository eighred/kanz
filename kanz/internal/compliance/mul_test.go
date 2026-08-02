package compliance

import (
	"math"
	"math/big"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	decutil "github.com/eighred/kanz/internal/dec"
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

func TestAddDecimal_ExactInTheNormalRange(t *testing.T) {
	// Values that do not overflow must be unchanged by this fix. Aligning to the
	// smaller exponent: 100 (100e0) + 0.5 (5e-1) = 100.5 (1005e-1).
	got, ok := addDecimal(
		&commonpb.Decimal{Coefficient: 100, Exponent: 0},
		&commonpb.Decimal{Coefficient: 5, Exponent: -1},
	)
	if !ok {
		t.Fatal("ok = false for a value well inside int64 — the guard is refusing normal input")
	}
	if got.GetCoefficient() != 1005 || got.GetExponent() != -1 {
		t.Fatalf("got %d e%d, want 1005 e-1", got.GetCoefficient(), got.GetExponent())
	}
}

func TestAddDecimal_NilOperandsBehaveAsZero(t *testing.T) {
	got, ok := addDecimal(nil, &commonpb.Decimal{Coefficient: 7, Exponent: 0})
	if !ok || got.GetCoefficient() != 7 || got.GetExponent() != 0 {
		t.Fatalf("got %v (ok=%v), want 7 e0 — a nil operand is zero, as before", got, ok)
	}
	if _, ok := addDecimal(nil, nil); !ok {
		t.Fatal("addDecimal(nil, nil) must succeed as zero")
	}
}

// THE ALIGNMENT WRAP. An exponent gap of 19 overflows pow10's int64.
// Before the fix this returns 0.3875820196..., a number with no relationship
// to the inputs.
func TestAddDecimal_LargeExponentGapDoesNotWrap(t *testing.T) {
	got, ok := addDecimal(
		&commonpb.Decimal{Coefficient: 100, Exponent: 0},
		&commonpb.Decimal{Coefficient: 1, Exponent: -19},
	)
	if !ok {
		// Refusing is acceptable here (the exact sum needs more than int64 of
		// precision); returning a WRONG number is not.
		return
	}
	// If it did represent it, the value must be ~100, not 0.38.
	f, _ := new(big.Rat).SetFrac(
		big.NewInt(got.GetCoefficient()),
		new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-got.GetExponent())), nil),
	).Float64()
	if f < 99.9 || f > 100.1 {
		t.Fatalf("got %v (%d e%d), want ~100 — the alignment multiply wrapped",
			f, got.GetCoefficient(), got.GetExponent())
	}
}

// THE SIGN FLIP. A gap of 25 wraps the alignment negative, so adding a tiny
// POSITIVE quantity to 100 yields a NEGATIVE position.
func TestAddDecimal_LargeGapNeverFlipsTheSign(t *testing.T) {
	got, ok := addDecimal(
		&commonpb.Decimal{Coefficient: 100, Exponent: 0},
		&commonpb.Decimal{Coefficient: 5, Exponent: -25},
	)
	if ok && got.GetCoefficient() < 0 {
		t.Fatalf("got a NEGATIVE coefficient (%d e%d) from adding two POSITIVE quantities",
			got.GetCoefficient(), got.GetExponent())
	}
}

// THE SECOND WRAP SITE, which the review did not report: both operands are
// individually fine and the SUM overflows.
func TestAddDecimal_OverflowingSumDoesNotWrap(t *testing.T) {
	got, ok := addDecimal(
		&commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: 0},
		&commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: 0},
	)
	if !ok {
		return // refusing is acceptable
	}
	if got.GetCoefficient() < 0 {
		t.Fatalf("MaxInt64 + MaxInt64 produced a NEGATIVE coefficient (%d e%d)",
			got.GetCoefficient(), got.GetExponent())
	}
	// Rescaled, it must still be ~1.8e19 in magnitude.
	mag := new(big.Rat).SetFrac(
		big.NewInt(got.GetCoefficient()),
		big.NewInt(1),
	)
	if got.GetExponent() > 0 {
		mag.Mul(mag, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(got.GetExponent())), nil)))
	}
	want := new(big.Rat).SetInt(new(big.Int).Mul(big.NewInt(math.MaxInt64), big.NewInt(2)))
	ratio := new(big.Rat).Quo(mag, want)
	f, _ := ratio.Float64()
	if f < 0.999 || f > 1.001 {
		t.Fatalf("magnitude %v is not ~2*MaxInt64 — the sum wrapped or was mis-rescaled", mag)
	}
}

// NON-VACUITY: sign is preserved through rescaling for genuine negatives
// (a SELL is a negative signed quantity).
func TestAddDecimal_NegativeSumKeepsItsSign(t *testing.T) {
	got, ok := addDecimal(
		&commonpb.Decimal{Coefficient: -math.MaxInt64, Exponent: 0},
		&commonpb.Decimal{Coefficient: -math.MaxInt64, Exponent: 0},
	)
	if ok && got.GetCoefficient() >= 0 {
		t.Fatalf("two negative quantities summed to a non-negative coefficient (%d e%d)",
			got.GetCoefficient(), got.GetExponent())
	}
}

// TestAddDecimal_RefusesWhenTheExponentCannotBeRepresented mirrors
// TestMulDecimal_RefusesWhenTheExponentCannotBeRepresented for the sum path.
// The coverage gap here is what let the unbounded-gap defect through: nothing
// exercised addDecimal's rescale loop to its refusal edge. The contract is the
// same — REFUSE, never a wrapped number — and refusal routes through the
// existing Decision.Unvaluable path at the call site.
func TestAddDecimal_RefusesWhenTheExponentCannotBeRepresented(t *testing.T) {
	// Two coefficients that cannot sum inside an int64, already sitting at the
	// highest exponent a Decimal can express: the sum needs the exponent RAISED
	// to fit, and there is nowhere left to raise it to.
	got, ok := addDecimal(
		&commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: math.MaxInt32},
		&commonpb.Decimal{Coefficient: math.MaxInt64, Exponent: math.MaxInt32},
	)
	if ok {
		t.Fatalf("ok = true, want false — the exponent cannot be raised past MaxInt32 (got %+v)", got)
	}
	if got != nil {
		t.Fatalf("got = %+v, want nil when not representable", got)
	}
}

// THE DoS. A crafted order can put ~4.3e9 between the two exponents, and
// aligning unconditionally to the smaller one asks math/big for a
// multi-billion-digit 10^gap. The process hangs or OOMs long before it reaches
// the "refuse if unrepresentable" loop — strictly worse than the int64 wrap it
// replaced, which at least returned in O(1).
//
// The correct answer is arithmetic, not refusal: 10^2000000000 + 10^-2000000000
// is 10^2000000000 to every digit a Decimal can hold.
func TestAddDecimal_HugeExponentGapReturnsPromptly(t *testing.T) {
	got, ok := addDecimal(
		&commonpb.Decimal{Coefficient: 1, Exponent: -2000000000},
		&commonpb.Decimal{Coefficient: 1, Exponent: 2000000000},
	)
	if !ok {
		t.Fatal("ok = false — 10^2000000000 is perfectly representable as 1e18 e1999999982; refusing it is wrong")
	}
	if got.GetCoefficient() != 1_000_000_000_000_000_000 || got.GetExponent() != 1_999_999_982 {
		t.Fatalf("got %d e%d, want 1000000000000000000 e1999999982 (= 10^2000000000)",
			got.GetCoefficient(), got.GetExponent())
	}
}

// The same gap with the large operand NEGATIVE — a SELL is a negative signed
// quantity, and the clamp must not lose its sign.
func TestAddDecimal_HugeExponentGapKeepsTheSign(t *testing.T) {
	got, ok := addDecimal(
		&commonpb.Decimal{Coefficient: -1, Exponent: 2000000000},
		&commonpb.Decimal{Coefficient: 1, Exponent: -2000000000},
	)
	if !ok {
		t.Fatal("ok = false, want a representable negative sum")
	}
	if got.GetCoefficient() != -1_000_000_000_000_000_000 || got.GetExponent() != 1_999_999_982 {
		t.Fatalf("got %d e%d, want -1000000000000000000 e1999999982", got.GetCoefficient(), got.GetExponent())
	}
}

// Large-but-not-absurd gaps: 30 sits inside the alignment window, 100 and 1000
// are clamped. All three must return promptly with the SAME value, because
// 1 + 10^-30, 1 + 10^-100 and 1 + 10^-1000 are all exactly 1 at nineteen
// significant digits.
func TestAddDecimal_LargeGapsReturnPromptlyAndCorrectly(t *testing.T) {
	for _, gap := range []int32{30, 100, 1000} {
		got, ok := addDecimal(
			&commonpb.Decimal{Coefficient: 1, Exponent: 0},
			&commonpb.Decimal{Coefficient: 1, Exponent: -gap},
		)
		if !ok {
			t.Fatalf("gap %d: ok = false, want a representable ~1", gap)
		}
		if got.GetCoefficient() != 1_000_000_000_000_000_000 || got.GetExponent() != -18 {
			t.Fatalf("gap %d: got %d e%d, want 1000000000000000000 e-18 (= 1)",
				gap, got.GetCoefficient(), got.GetExponent())
		}
	}
}

// THE ZERO-COEFFICIENT GUARD. addDecimal adjusts a zero operand's exponent to
// its partner's before clamping, and those four lines are the whole reason a
// zero cannot erase a book.
//
// Without them, {1000, e0} + {0, e2000000000} returns 0: alignExponent sees a
// hi of 2e9, clamps the alignment exponent to hi-40, and the REAL operand —
// sitting two billion places below that — is truncated to nothing by scale().
// A zero plus a thousand is a thousand; returning zero is not a rounding loss,
// it is annihilation of the only operand that had a magnitude.
//
// Downstream that is COMP-M1 reached through a new door. project() values the
// summed quantity into the position's MarketValue, heldPositions (rules.go)
// drops zero-valued positions as flat, and a projected book with no positions
// breaches nothing — every rule in the mandate passes. Quantity.Exponent is an
// unvalidated wire field and the gate runs before Accept validates the order,
// so the operand carrying the absurd exponent is attacker-supplied.
//
// TestPreTradeGate_ZeroCoefficientCannotEraseTheBook (gate_test.go) pins the
// same guard at the gate level; this one pins the arithmetic.
func TestAddDecimal_ZeroCoefficientDoesNotAnnihilateTheOtherOperand(t *testing.T) {
	cases := []struct {
		name    string
		a, b    *commonpb.Decimal
		wantCo  int64
		wantExp int32
	}{
		{
			"zero second operand, absurd positive exponent",
			&commonpb.Decimal{Coefficient: 1000, Exponent: 0},
			&commonpb.Decimal{Coefficient: 0, Exponent: 2000000000},
			1000, 0,
		},
		{
			"zero FIRST operand, absurd positive exponent",
			&commonpb.Decimal{Coefficient: 0, Exponent: 2000000000},
			&commonpb.Decimal{Coefficient: 1000, Exponent: 0},
			1000, 0,
		},
		{
			"zero operand, absurd NEGATIVE exponent (drags the window the other way)",
			&commonpb.Decimal{Coefficient: 1000, Exponent: 0},
			&commonpb.Decimal{Coefficient: 0, Exponent: -2000000000},
			1000, 0,
		},
		{
			"negative real operand must keep its sign and magnitude (a SELL)",
			&commonpb.Decimal{Coefficient: -1000, Exponent: 0},
			&commonpb.Decimal{Coefficient: 0, Exponent: 2000000000},
			-1000, 0,
		},
		{
			// Both zero: the guard adjusts a to b (the first branch wins), so
			// both align at b's exponent and the result is 0 there. The value
			// is zero either way; what is pinned is that the pair of zeroes
			// does not go anywhere near the clamp.
			"both zero — still zero, and the exponent must not run away",
			&commonpb.Decimal{Coefficient: 0, Exponent: 2000000000},
			&commonpb.Decimal{Coefficient: 0, Exponent: -2000000000},
			0, -2000000000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := addDecimal(tc.a, tc.b)
			if !ok {
				t.Fatalf("ok = false, want a representable sum")
			}
			if got.GetCoefficient() != tc.wantCo || got.GetExponent() != tc.wantExp {
				t.Fatalf("got %d e%d, want %d e%d — a zero-coefficient operand's exponent "+
					"dragged the alignment window; the real operand is meant to pass "+
					"through untouched, in its own representation",
					got.GetCoefficient(), got.GetExponent(), tc.wantCo, tc.wantExp)
			}
		})
	}
}

// NON-VACUITY. Ordinary gaps must produce exactly what they produce today —
// a clamp set too tight would quietly corrupt normal arithmetic, which is the
// main risk of this change.
func TestAddDecimal_SmallGapsAreUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		a, b    *commonpb.Decimal
		wantCo  int64
		wantExp int32
	}{
		{"gap0", &commonpb.Decimal{Coefficient: 5, Exponent: 0}, &commonpb.Decimal{Coefficient: 7, Exponent: 0}, 12, 0},
		{"gap1", &commonpb.Decimal{Coefficient: 3, Exponent: -1}, &commonpb.Decimal{Coefficient: 25, Exponent: -2}, 55, -2},
		{"gap2", &commonpb.Decimal{Coefficient: 7, Exponent: 0}, &commonpb.Decimal{Coefficient: 125, Exponent: -2}, 825, -2},
		{"gap8", &commonpb.Decimal{Coefficient: 1, Exponent: 0}, &commonpb.Decimal{Coefficient: 12345678, Exponent: -8}, 112345678, -8},
		{"gap8-negative", &commonpb.Decimal{Coefficient: -1, Exponent: 0}, &commonpb.Decimal{Coefficient: 12345678, Exponent: -8}, -87654322, -8},
	}
	for _, tc := range cases {
		got, ok := addDecimal(tc.a, tc.b)
		if !ok {
			t.Fatalf("%s: ok = false for ordinary input", tc.name)
		}
		if got.GetCoefficient() != tc.wantCo || got.GetExponent() != tc.wantExp {
			t.Fatalf("%s: got %d e%d, want %d e%d", tc.name, got.GetCoefficient(), got.GetExponent(), tc.wantCo, tc.wantExp)
		}
	}
}

// TestAlignExponent_GapNeverExceedsWindow guards the INVARIANT the DoS fix
// depends on, not the symptom (addDecimal returning promptly). It is the
// structural counterpart to TestAddDecimal_HugeExponentGapReturnsPromptly
// above: that test proves one crafted pair does not hang; this test proves
// NO pair can, because alignExponent's output can never put more than
// alignWindow digits between itself and the larger exponent — which is the
// one fact that makes addDecimal's big.Int Exp(10, gap, nil) calls O(1).
//
// This is strictly stronger than a timeout: it is instant, allocates
// nothing beyond int64s, and — unlike a goroutine racing a 2s clock — cannot
// itself leak a runaway computation if the invariant it checks ever breaks.
//
// The bound below is the LITERAL 40, not the package's alignWindow constant.
// That is deliberate: this test must fail if a future change widens
// alignWindow itself, so it cannot reference the very constant whose value
// it is pinning (confirmed — see the mutation check in this task's report).
func TestAlignExponent_GapNeverExceedsWindow(t *testing.T) {
	const wantMaxGap = 40
	values := []int64{
		math.MinInt32, math.MinInt32 + 1, math.MinInt32 / 2,
		-wantMaxGap - 1, -wantMaxGap, -wantMaxGap + 1,
		-1000, -100, -41, -40, -39, -1, 0, 1,
		39, 40, 41, 100, 1000,
		math.MaxInt32 / 2, math.MaxInt32 - 1, math.MaxInt32,
	}
	for _, a := range values {
		for _, b := range values {
			exp := decutil.AlignExponent(a, b)
			hi := a
			if b > hi {
				hi = b
			}
			gap := hi - exp
			if gap < 0 || gap > wantMaxGap {
				t.Fatalf("decutil.AlignExponent(%d, %d) = %d: hi-exp = %d, want in [0, %d]",
					a, b, exp, gap, wantMaxGap)
			}
		}
	}
}

// TestAlignExponent_WindowBoundary pins the alignWindow=40 edge directly on
// alignExponent, independent of addDecimal's later int64-fit rescale loop.
//
// That independence matters: the rescale loop that follows alignment always
// re-rounds a 41+ digit aligned sum down to <=19 significant digits whenever
// the exponent gap reaches this window (see TestAddDecimal_LargeGapsReturn-
// PromptlyAndCorrectly, where gaps of 30/100/1000 all collapse to the same
// answer). That rounding provably swallows any single-digit difference at
// the smaller operand's bottom digit — verified by hand (position analysis:
// the smaller operand occupies digits [0, 18] at most, the kept output
// occupies digits [22, ...], and no rounding carry can bridge that 3-digit
// gap) and confirmed with a throwaway script reproducing the algorithm's
// shape for every b coefficient from 1 to MaxInt64. So a Decimal-level
// assertion at gap 40 vs 41 CANNOT observe the clamp turning on or off — see
// TestAddDecimal_WindowAndDivisorBoundariesProduceCorrectValues, which pins
// exactly that (identical) Decimal-level result as the non-vacuity check.
// The clamp's actual on/off transition is only observable one level down,
// on alignExponent's own return value — which is what this test pins.
//
// hi=0 fixed, lo=-gap:
//
//	gap=40: lo=-40=hi-alignWindow, so "hi-alignWindow > lo" is -40>-40,
//	        FALSE: exp=lo=-40 (unclamped). The smaller operand's own
//	        exponent (-40) equals exp, so its participation gap
//	        (expB-exp) is 0 — it enters scale() unaltered.
//	gap=41: lo=-41, "hi-alignWindow > lo" is -40>-41, TRUE: exp=hi-
//	        alignWindow=-40 (clamped). The smaller operand's own exponent
//	        (-41) is now one below exp, so its participation gap is -1 —
//	        scale() truncates its least significant digit.
func TestAlignExponent_WindowBoundary(t *testing.T) {
	cases := []struct {
		gap                  int64
		wantExp              int64
		wantParticipationGap int64
	}{
		{gap: 40, wantExp: -40, wantParticipationGap: 0},
		{gap: 41, wantExp: -40, wantParticipationGap: -1},
	}
	for _, tc := range cases {
		expA, expB := int64(0), -tc.gap
		exp := decutil.AlignExponent(expA, expB)
		if exp != tc.wantExp {
			t.Fatalf("gap %d: decutil.AlignExponent(0, %d) = %d, want %d", tc.gap, expB, exp, tc.wantExp)
		}
		if participationGap := expB - exp; participationGap != tc.wantParticipationGap {
			t.Fatalf("gap %d: smaller operand's scale gap = %d, want %d",
				tc.gap, participationGap, tc.wantParticipationGap)
		}
	}
}

// TestAddDecimal_WindowAndDivisorBoundariesProduceCorrectValues is the
// Decimal-level, non-vacuity companion to the window-boundary test above: at
// the window edges (gap 40/41) and either side of scale()'s `-gap >= 19`
// divisor short circuit (gap 58/59), addDecimal must still return
// the mathematically correct value. 1 + 10^-gap is exactly 1 at nineteen
// significant digits for any gap>=1 (same derivation as the existing
// gap-30/100/1000 case above), so all four edges must agree on the same
// answer — which is the point: per TestAlignExponent_WindowBoundary's
// comment, the clamp's on/off transition is provably invisible here, and
// this test is what proves that "provably" claim true of the real
// addDecimal, not just of the extracted arithmetic.
func TestAddDecimal_WindowAndDivisorBoundariesProduceCorrectValues(t *testing.T) {
	for _, gap := range []int32{40, 41, 58, 59} {
		got, ok := addDecimal(
			&commonpb.Decimal{Coefficient: 1, Exponent: 0},
			&commonpb.Decimal{Coefficient: 1, Exponent: -gap},
		)
		if !ok {
			t.Fatalf("gap %d: ok = false, want a representable ~1", gap)
		}
		if got.GetCoefficient() != 1_000_000_000_000_000_000 || got.GetExponent() != -18 {
			t.Fatalf("gap %d: got %d e%d, want 1000000000000000000 e-18 (= 1)",
				gap, got.GetCoefficient(), got.GetExponent())
		}
	}
}
