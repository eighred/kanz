package dec

import (
	"math"
	"math/big"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

func d(c int64, e int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: c, Exponent: e}
}

// TestArithmeticIsTotalOnInDomainInputs is the claim internal/risk/compute
// leans on when it turns a refusal into a nil measure: for inputs that passed a
// domain check (|exponent| ≤ maxSafeExponent, what dec.InDomain enforces at
// every ingress), Add, Mul and Abs NEVER refuse.
//
// The reasoning it pins: Mul's exponent starts inside ±128 and fit raises it by
// at most the ~20 digits an int64 product can exceed an int64 by; Add's starts
// inside ±128 and fit raises it by at most ~41 (alignWindow plus the same 20).
// Both land nowhere near MaxInt32. So a nil from compute means an INGRESS guard
// went missing, not that the arithmetic gave up — which is why compute treats it
// as an absent measure rather than a value it could fall back on.
func TestArithmeticIsTotalOnInDomainInputs(t *testing.T) {
	exps := []int32{-maxSafeExponent, -maxSafeExponent + 1, -64, -14, -8, -1, 0, 1, 8, 64, maxSafeExponent}
	coeffs := []int64{
		math.MinInt64, math.MinInt64 + 1, -math.MaxInt64, -1_000_000_000_000_000_000,
		-1, 0, 1, 1_000_000_000_000_000_000, math.MaxInt64 - 1, math.MaxInt64,
	}
	for _, ea := range exps {
		for _, eb := range exps {
			for _, ca := range coeffs {
				for _, cb := range coeffs {
					a, b := d(ca, ea), d(cb, eb)
					if !InDomain(a) || !InDomain(b) {
						t.Fatalf("test inputs must be in-domain: %v %v", a, b)
					}
					if _, ok := Add(a, b); !ok {
						t.Fatalf("Add(%d e%d, %d e%d) refused an IN-DOMAIN pair — "+
							"compute turns this into an absent measure, so it must be unreachable", ca, ea, cb, eb)
					}
					if _, ok := Mul(a, b); !ok {
						t.Fatalf("Mul(%d e%d, %d e%d) refused an IN-DOMAIN pair", ca, ea, cb, eb)
					}
					if _, ok := Abs(a); !ok {
						t.Fatalf("Abs(%d e%d) refused an IN-DOMAIN value", ca, ea)
					}
				}
			}
		}
	}
}

// TestAbs_MinInt64KeepsMagnitudeAndSign covers the one coefficient for which
// negation is not arithmetic: Go's -MinInt64 is MinInt64, so `-c` returns a
// NEGATIVE absolute value. Folded into a gross exposure it SUBTRACTS the
// position it was meant to enlarge — the same sign flip Mul had, one operator
// over.
func TestAbs_MinInt64KeepsMagnitudeAndSign(t *testing.T) {
	got, ok := Abs(d(math.MinInt64, -8))
	if !ok {
		t.Fatal("Abs(MinInt64 e-8) refused — |MinInt64| is representable one exponent coarser")
	}
	if got.GetCoefficient() < 0 {
		t.Fatalf("Abs(MinInt64) = %d — a NEGATIVE absolute value", got.GetCoefficient())
	}
	// |MinInt64| is 9223372036854775808, one past MaxInt64, so fit raises the
	// exponent one digit (half-up): 922337203685477581 at e-7.
	if want := "92233720368.5477581"; Str(FromProto(got)) != want {
		t.Fatalf("Abs(MinInt64 e-8) = %s, want %s", Str(FromProto(got)), want)
	}
}

// TestMul_RefusesWhenTheExponentCannotMove is the ok=false arm. It is
// unreachable behind a domain check (the test above), and it exists so that a
// path without one gets a REFUSAL rather than a fabricated coefficient.
func TestMul_RefusesWhenTheExponentCannotMove(t *testing.T) {
	if _, ok := Mul(d(math.MaxInt64, math.MaxInt32), d(math.MaxInt64, 0)); ok {
		t.Fatal("Mul returned a value at an exponent it cannot express — the caller would act on a fabricated number")
	}
	if _, ok := Mul(d(1, math.MinInt32), d(1, math.MinInt32)); ok {
		t.Fatal("Mul returned a value below MinInt32 exponent; rescaling only raises, so there is no way back")
	}
}

// TestAddAndMulReturnPromptlyOnWildExponents is the DoS arm. Decimal.exponent is
// a wire field; before the alignment clamp, `Add` at a gap of billions asked
// math/big for a multi-billion-digit bignum and the process stopped answering
// while still reporting healthy.
func TestAddAndMulReturnPromptlyOnWildExponents(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		Add(d(1, 2_000_000_000), d(1, -2_000_000_000))             //nolint:errcheck // asserting termination, not the value
		Add(d(math.MaxInt64, -2_000_000_000), d(1, 0))             //nolint:errcheck // ditto
		Mul(d(math.MaxInt64, -2_000_000_000), d(7, 2_000_000_000)) //nolint:errcheck // ditto
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Add/Mul did not return within 5s on wild exponents — the alignment clamp is gone " +
			"and one crafted value stalls whichever service touched it")
	}
}

// TestAddIsExactInsideTheWindow is the non-vacuity companion: the clamp and the
// rescale must be inert on ordinary values, or every existing number moves.
func TestAddIsExactInsideTheWindow(t *testing.T) {
	cases := []struct {
		a, b *commonpb.Decimal
		want string
	}{
		{d(150, -2), d(75, -2), "2.25"},
		{d(150, -2), d(25, -3), "1.525"},
		{d(1, 0), d(-200_000, -6), "0.8"},
		{d(0, -60), d(42, 0), "42"}, // a zero operand's exponent must not drag the window
	}
	for _, tc := range cases {
		got, ok := Add(tc.a, tc.b)
		if !ok {
			t.Fatalf("Add(%v, %v) refused", tc.a, tc.b)
		}
		if s := Str(FromProto(got)); s != tc.want {
			t.Fatalf("Add(%v, %v) = %s, want %s", tc.a, tc.b, s, tc.want)
		}
	}
}

// TestFitRoundsHalfUpAwayFromZero pins the direction of the digit fit drops. It
// is the only rounding these operators introduce, and it has to match
// ToProtoScaled's — which now IS this function, which is the point of the move.
func TestFitRoundsHalfUpAwayFromZero(t *testing.T) {
	// 10^19 + 5: one digit past int64, with the dropped digit exactly 5.
	pos, _ := new(big.Int).SetString("10000000000000000005", 10)
	got, ok := fit(pos, 0)
	if !ok || got.GetCoefficient() != 1000000000000000001 || got.GetExponent() != 1 {
		t.Fatalf("fit(1.0000000000000000005e19, 0) = %v (ok=%v), want 1000000000000000001 e1", got, ok)
	}
	neg, _ := new(big.Int).SetString("-10000000000000000005", 10)
	got, ok = fit(neg, 0)
	if !ok || got.GetCoefficient() != -1000000000000000001 {
		t.Fatalf("fit of the negative half rounded toward zero: %v (ok=%v) — half-up must be AWAY from zero", got, ok)
	}
}
