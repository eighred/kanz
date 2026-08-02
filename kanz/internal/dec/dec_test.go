package dec

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

func TestToProtoScaled_NormalValueIsUnchangedAtScale8(t *testing.T) {
	got, ok := ToProtoScaled(big.NewRat(12345, 100)) // 123.45
	if !ok {
		t.Fatal("ok = false for a value well inside int64")
	}
	if got.GetExponent() != -8 || got.GetCoefficient() != 12345000000 {
		t.Fatalf("got %d e%d, want 12345000000 e-8", got.GetCoefficient(), got.GetExponent())
	}
}

// The $92bn ceiling: at scale -8 this coefficient does not fit an int64.
// ToProto WRAPS here; ToProtoScaled must keep the magnitude instead.
func TestToProtoScaled_LargeValueRescalesInsteadOfWrapping(t *testing.T) {
	v := new(big.Rat).SetInt64(100_000_000_000) // $100bn
	got, ok := ToProtoScaled(v)
	if !ok {
		t.Fatal("ok = false for $100bn — a real institutional position must not refuse")
	}
	if got.GetCoefficient() < 0 {
		t.Fatalf("negative coefficient %d — it wrapped", got.GetCoefficient())
	}
	// Reconstruct and compare: must be within one unit of the last kept digit.
	back := new(big.Rat).SetInt64(got.GetCoefficient())
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-got.GetExponent())), nil)
	back.Quo(back, new(big.Rat).SetInt(pow))
	diff := new(big.Rat).Sub(back, v)
	if diff.Abs(diff).Cmp(big.NewRat(1, 1)) > 0 {
		t.Fatalf("reconstructed %s differs from %s by more than 1", back.FloatString(2), v.FloatString(2))
	}
}

func TestToProtoScaled_NegativeLargeValueKeepsItsSign(t *testing.T) {
	got, ok := ToProtoScaled(new(big.Rat).SetInt64(-100_000_000_000))
	if !ok {
		t.Fatal("ok = false for -$100bn")
	}
	if got.GetCoefficient() >= 0 {
		t.Fatalf("coefficient %d is not negative — the sign was lost", got.GetCoefficient())
	}
}

// NON-VACUITY: ToProtoScaled must agree with ToProto wherever BOTH are
// representable. This is the test retargeted from the deleted exact-or-refuse
// conversion this function replaces — it is what protects ToProto's ~30
// remaining callers from a regression in the shared scaledCoefficient helper.
func TestToProtoScaled_AgreesWithToProtoWhereBothFit(t *testing.T) {
	for _, r := range []*big.Rat{
		big.NewRat(0, 1), big.NewRat(1, 1), big.NewRat(-1, 1),
		big.NewRat(12345, 100), big.NewRat(-12345, 100),
		big.NewRat(1, 3), big.NewRat(-1, 3),
		big.NewRat(999999999, 1),
	} {
		want := ToProto(r)
		got, ok := ToProtoScaled(r)
		if !ok {
			t.Fatalf("%s: ToProtoScaled refused a representable value", r.FloatString(4))
		}
		if got.GetCoefficient() != want.GetCoefficient() || got.GetExponent() != want.GetExponent() {
			t.Fatalf("%s: ToProtoScaled = %d e%d, ToProto = %d e%d — they must agree where both fit",
				r.FloatString(4), got.GetCoefficient(), got.GetExponent(),
				want.GetCoefficient(), want.GetExponent())
		}
	}
}

func TestToProtoScaled_NilIsZero(t *testing.T) {
	got, ok := ToProtoScaled(nil)
	if !ok || got.GetCoefficient() != 0 {
		t.Fatalf("got %v (ok=%v), want zero", got, ok)
	}
}

// TestToProto_LiteralOutput pins ToProto's ABSOLUTE output.
//
// TestToProtoScaled_AgreesWithToProtoWhereBothFit above cannot fail while both
// functions delegate to scaledCoefficient, and it says nothing about the
// overflow region where the two are DESIGNED to differ. The real regression
// risk is that scaledCoefficient changes and silently alters ToProto for
// every caller in the repository. This table is what makes that impossible:
// it names the expected coefficient and exponent outright, including
// rounding ties in both signs, values against the int64 boundary, and —
// deliberately — one overflowing value pinning that ToProto STILL WRAPS
// exactly as it always has. That wrapping is preserved on purpose for
// existing callers; ToProtoScaled is the variant a capital path must use.
// TestFromProtoChecked_AbsurdExponentRefusesPromptly guards the fix for the
// live hang: FromProto materialises 10^abs(exponent) with no bound, and a
// MarketDataEvent price at exponent 2000000000, taken directly off the wire
// by internal/marketdata/mark, hangs the fold indefinitely.
// FromProtoChecked must refuse instead of computing.
//
// Guarded STRUCTURALLY rather than with a timeout: a timeout bounds the
// test, not the process, and Go cannot cancel a goroutine still grinding a
// multi-billion-digit bignum. If the bound regresses, this call never
// returns and CI fails on its own panic timeout — which is loud, but the
// real protection is that FromProtoChecked's domain check is O(1) by
// construction, the same reasoning internal/compliance's decimalInDomain
// relies on for the equivalent bound at the rules engine.
func TestFromProtoChecked_AbsurdExponentRefusesPromptly(t *testing.T) {
	for _, exp := range []int32{2000000000, -2000000000} {
		done := make(chan struct {
			ok bool
		})
		go func(e int32) {
			_, ok := FromProtoChecked(&commonpb.Decimal{Coefficient: 1, Exponent: e})
			done <- struct{ ok bool }{ok}
		}(exp)
		select {
		case r := <-done:
			if r.ok {
				t.Fatalf("exponent %d: ok = true, want false", exp)
			}
		case <-time.After(4 * time.Second):
			t.Fatalf("exponent %d: FromProtoChecked did not return within 4s — the bound regressed", exp)
		}
	}
}

// TestFromProtoChecked_BoundaryInBothSigns pins the exact domain: 64 mirrors
// internal/compliance's maxDecimalExponent, reused as a number (not an
// import — dec and compliance must not depend on each other).
func TestFromProtoChecked_BoundaryInBothSigns(t *testing.T) {
	for _, tc := range []struct {
		name string
		exp  int32
		ok   bool
	}{
		{"at the positive bound", 64, true},
		{"past the positive bound", 65, false},
		{"at the negative bound", -64, true},
		{"past the negative bound", -65, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := FromProtoChecked(&commonpb.Decimal{Coefficient: 1, Exponent: tc.exp})
			if ok != tc.ok {
				t.Fatalf("exponent %d: ok = %v, want %v", tc.exp, ok, tc.ok)
			}
		})
	}
}

// NON-VACUITY. Every guard above fails closed on absurd input, so a version
// of FromProtoChecked that refused EVERYTHING would satisfy them all.
// Ordinary values — a real price, zero, a negative — must still convert and
// must agree with FromProto exactly, or the checked variant would be an
// independent (and possibly diverging) implementation rather than a guard
// in front of the real one.
func TestFromProtoChecked_AgreesWithFromProtoForOrdinaryValues(t *testing.T) {
	for _, d := range []*commonpb.Decimal{
		{Coefficient: 12345, Exponent: -2}, // 123.45
		{Coefficient: 0, Exponent: 0},      // zero
		{Coefficient: -500, Exponent: 0},   // negative
		nil,                                // absent, same as FromProto
	} {
		want := FromProto(d)
		got, ok := FromProtoChecked(d)
		if !ok {
			t.Fatalf("%v: ok = false for an ordinary value", d)
		}
		if got.Cmp(want) != 0 {
			t.Fatalf("%v: FromProtoChecked = %s, want %s (FromProto)", d, got.FloatString(4), want.FloatString(4))
		}
	}
}

func TestToProto_LiteralOutput(t *testing.T) {
	cases := []struct {
		name      string
		in        *big.Rat
		wantCoeff int64
		wantExp   int32
	}{
		{"zero", big.NewRat(0, 1), 0, -8},
		{"one", big.NewRat(1, 1), 100000000, -8},
		{"smallest representable", big.NewRat(1, 100000000), 1, -8},
		{"negative smallest", big.NewRat(-1, 100000000), -1, -8},
		{"third truncates and rounds up", big.NewRat(1, 3), 33333333, -8},
		{"negative third", big.NewRat(-1, 3), -33333333, -8},
		{"two thirds rounds up", big.NewRat(2, 3), 66666667, -8},
		{"negative two thirds rounds away from zero", big.NewRat(-2, 3), -66666667, -8},

		// Rounding TIES, both signs: half-up is away from zero.
		{"positive tie rounds up", Rat("0.000000005"), 1, -8},
		{"negative tie rounds down", Rat("-0.000000005"), -1, -8},
		{"positive tie mid-magnitude", Rat("1.234567895"), 123456790, -8},
		{"negative tie mid-magnitude", Rat("-1.234567895"), -123456790, -8},
		{"just below a tie", Rat("0.123456784"), 12345678, -8},
		{"just above a tie", Rat("0.123456786"), 12345679, -8},

		// The int64 boundary. 92233720368.54775807 × 10^8 is exactly MaxInt64.
		{"exactly MaxInt64 scaled", Rat("92233720368.54775807"), 9223372036854775807, -8},
		{"exactly MinInt64 scaled", Rat("-92233720368.54775808"), -9223372036854775808, -8},

		// OVERFLOW: ToProto wraps (big.Int.Int64()), by design, for existing
		// callers. This literal is the tripwire on that documented behaviour.
		{"overflow wraps", big.NewRat(184467440738, 1), 90448384, -8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ToProto(tc.in)
			if got.GetCoefficient() != tc.wantCoeff || got.GetExponent() != tc.wantExp {
				t.Fatalf("ToProto(%s) = {coeff:%d exp:%d}, want {coeff:%d exp:%d}",
					tc.in.String(), got.GetCoefficient(), got.GetExponent(), tc.wantCoeff, tc.wantExp)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The predicates (#95). These used to answer via FromProto and therefore
// inherited its hang on an unvalidated wire exponent.
// ---------------------------------------------------------------------------

// THE REGRESSION THIS EXISTS FOR. dec.IsPositive(cmd.GetQuantity()) on an order
// carrying {1, 2000000000} did not return, and that call sits in the OMS order
// aggregate with no domain gate in front of it.
//
// Structurally guarded, like TestFromProtoChecked_AbsurdExponentRefusesPromptly:
// if this regresses the call never returns and CI fails on its own panic
// timeout. The real protection is that none of these three reads the exponent
// in a way that can materialise it.
func TestPredicatesAreTotalOnAbsurdExponents(t *testing.T) {
	huge := &commonpb.Decimal{Coefficient: 1, Exponent: 2000000000}
	hugeNeg := &commonpb.Decimal{Coefficient: -1, Exponent: -2000000000}

	for _, tc := range []struct {
		name string
		call func() bool
		want bool
	}{
		{"IsPositive(+1e2000000000)", func() bool { return IsPositive(huge) }, true},
		{"IsPositive(-1e-2000000000)", func() bool { return IsPositive(hugeNeg) }, false},
		{"IsZero(+1e2000000000)", func() bool { return IsZero(huge) }, false},
		{"Cmp is signed by coefficient", func() bool { return Cmp(huge, hugeNeg) > 0 }, true},
		{"Cmp of two absurd exponents", func() bool { return Cmp(huge, huge) == 0 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan bool, 1)
			go func() { done <- tc.call() }()
			select {
			case got := <-done:
				if got != tc.want {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			case <-time.After(4 * time.Second):
				t.Fatalf("%s did not return within 4s — the predicate is materialising 10^exponent again", tc.name)
			}
		})
	}
}

// NOT A NEW IMPLEMENTATION — THE SAME ANSWERS, ARRIVED AT WITHOUT EXPANDING.
//
// This is the non-vacuity guard for the three above: a version that answered
// "false, false, 0" for everything would pass the totality test. Every pair in
// the in-domain cross-product must give byte-identical results to the expanding
// definition these functions used to have, or the fix silently changed what an
// order comparison means.
func TestPredicatesAgreeWithTheExpandingDefinition(t *testing.T) {
	coeffs := []int64{0, 1, -1, 5, -5, 999, -999, 1000, 123456789, -123456789}
	exps := []int32{-8, -3, -1, 0, 1, 3, 8, 64, -64}

	var vals []*commonpb.Decimal
	for _, c := range coeffs {
		for _, e := range exps {
			vals = append(vals, &commonpb.Decimal{Coefficient: c, Exponent: e})
		}
	}
	vals = append(vals, nil)

	for _, a := range vals {
		// The expanding definitions, written out: what these functions were.
		if got, want := IsPositive(a), FromProto(a).Sign() > 0; got != want {
			t.Fatalf("IsPositive(%v) = %v, want %v", a, got, want)
		}
		if got, want := IsZero(a), FromProto(a).Sign() == 0; got != want {
			t.Fatalf("IsZero(%v) = %v, want %v", a, got, want)
		}
		for _, b := range vals {
			if got, want := Cmp(a, b), FromProto(a).Cmp(FromProto(b)); got != want {
				t.Fatalf("Cmp(%v, %v) = %d, want %d", a, b, got, want)
			}
		}
	}
}

// The alignment branch of cmpMagnitude is the only one that multiplies, and it
// is reached only on a tie in decimal order. The cross-product above covers it
// incidentally; these name the cases so a regression says WHICH property broke.
func TestCmpAlignsOnlyWhenDecimalOrdersTie(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b *commonpb.Decimal
		want int
	}{
		// 1e3 = 1000 vs 999e1 = 9990. Orders tie at 4; alignment decides.
		{"tie in order, alignment decides", &commonpb.Decimal{Coefficient: 1, Exponent: 3},
			&commonpb.Decimal{Coefficient: 999, Exponent: 1}, -1},
		// 1e5 = 100000 (order 6) vs 999e1 = 9990 (order 4). No alignment needed.
		{"order alone decides", &commonpb.Decimal{Coefficient: 1, Exponent: 5},
			&commonpb.Decimal{Coefficient: 999, Exponent: 1}, 1},
		// Same value, different representations: 1.5 as 15e-1 and 150e-2.
		{"equal values written differently", &commonpb.Decimal{Coefficient: 15, Exponent: -1},
			&commonpb.Decimal{Coefficient: 150, Exponent: -2}, 0},
		// Negatives invert the magnitude comparison.
		{"more negative is smaller", &commonpb.Decimal{Coefficient: -999, Exponent: 1},
			&commonpb.Decimal{Coefficient: -1, Exponent: 3}, -1},
		// Zero compares equal whatever exponent it carries.
		{"zero equals zero across exponents", &commonpb.Decimal{Coefficient: 0, Exponent: 64},
			&commonpb.Decimal{Coefficient: 0, Exponent: -64}, 0},
		// A sign difference short-circuits before magnitude is looked at.
		{"sign decides before magnitude", &commonpb.Decimal{Coefficient: -1, Exponent: 60},
			&commonpb.Decimal{Coefficient: 1, Exponent: -60}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Cmp(tc.a, tc.b); got != tc.want {
				t.Fatalf("Cmp = %d, want %d", got, tc.want)
			}
			if got := Cmp(tc.b, tc.a); got != -tc.want {
				t.Fatalf("Cmp reversed = %d, want %d — the comparison is not antisymmetric", got, -tc.want)
			}
		})
	}
}

// InDomain is the predicate FromProtoChecked answers with; they must not drift.
func TestInDomainMatchesFromProtoChecked(t *testing.T) {
	for _, exp := range []int32{-65, -64, -1, 0, 1, 64, 65} {
		d := &commonpb.Decimal{Coefficient: 1, Exponent: exp}
		_, ok := FromProtoChecked(d)
		if InDomain(d) != ok {
			t.Fatalf("exponent %d: InDomain = %v but FromProtoChecked ok = %v", exp, InDomain(d), ok)
		}
	}
	if !InDomain(nil) {
		t.Error("a nil Decimal is absent, not out of range — InDomain(nil) must be true")
	}
}

// MoneyIn is the ONE guard on netting a Money against a balance (#221). The
// arithmetic cannot see a currency, so nothing else stops a BTC fee being
// subtracted from a USD cash leg — a number that is not imprecise but wrong, in
// an append-only book.
func TestMoneyIn(t *testing.T) {
	money := func(coeff int64, exp int32, ccy string) *commonpb.Money {
		return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: coeff, Exponent: exp}, CurrencyCode: ccy}
	}
	cases := []struct {
		name     string
		m        *commonpb.Money
		currency string
		want     *big.Rat // nil ⇒ must refuse
	}{
		{"matching currency", money(5, 0, "USD"), "USD", big.NewRat(5, 1)},
		{"other currency", money(8, -4, "BTC"), "USD", nil},
		{"unstamped non-zero", money(5, 0, ""), "USD", nil},
		{"nil money", nil, "USD", new(big.Rat)},
		{"zero in another currency", money(0, 0, "BTC"), "USD", new(big.Rat)},
		{"negative in currency", money(-250, -2, "USD"), "USD", big.NewRat(-250, 100)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MoneyIn(tc.m, tc.currency)
			if tc.want == nil {
				if err == nil {
					t.Fatalf("want refusal, got %s", got.RatString())
				}
				if !errors.Is(err, ErrCurrencyMismatch) {
					t.Fatalf("want ErrCurrencyMismatch, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if got.Cmp(tc.want) != 0 {
				t.Fatalf("got %s want %s", got.RatString(), tc.want.RatString())
			}
		})
	}
}

// The refusal must name the two currencies and the amount: an operator reading a
// DLQ has only this string to decide whether to re-drive or to correct the venue
// mapping.
func TestMoneyInRefusalNamesTheCurrencies(t *testing.T) {
	_, err := MoneyIn(&commonpb.Money{
		Amount:       &commonpb.Decimal{Coefficient: 8, Exponent: -4},
		CurrencyCode: "BTC",
	}, "USD")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"0.0008", `"BTC"`, `"USD"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q must name %s", err, want)
		}
	}
}
