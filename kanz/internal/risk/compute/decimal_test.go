package compute

import (
	"math"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	// decutil: this file already declares a local `dec(...)` Decimal-literal
	// helper, so the platform decimal package is aliased rather than renaming a
	// helper used across the package's tests.
	decutil "github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/risk/domain"
)

func dec(c int64, e int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: c, Exponent: e}
}

// wantValue asserts d's VALUE, not its representation. The magnitude-preserving
// rule deliberately trades exponent for headroom, so pinning a coefficient/
// exponent pair would fail on a correct answer expressed one digit coarser.
func wantValue(t *testing.T, d *commonpb.Decimal, want string, what string) {
	t.Helper()
	if d == nil {
		t.Fatalf("%s: got nil (unrepresentable), want %s", what, want)
	}
	got := decutil.Str(decutil.FromProto(d))
	if got != want {
		t.Fatalf("%s = %s (coefficient %d, exponent %d), want %s",
			what, got, d.GetCoefficient(), d.GetExponent(), want)
	}
}

func TestAddDecimal_SameExponent(t *testing.T) {
	got := addDecimal(dec(150, -2), dec(75, -2))
	if got.Coefficient != 225 || got.Exponent != -2 {
		t.Errorf("got %d × 10^%d want 225 × 10^-2", got.Coefficient, got.Exponent)
	}
}

func TestAddDecimal_DifferentExponentsAlignsToMorePrecise(t *testing.T) {
	// 1.50 + 0.025 = 1.525
	got := addDecimal(dec(150, -2), dec(25, -3))
	if got.Coefficient != 1525 || got.Exponent != -3 {
		t.Errorf("got %d × 10^%d want 1525 × 10^-3", got.Coefficient, got.Exponent)
	}
}

func TestAddDecimal_NilOperandsTreatedAsZero(t *testing.T) {
	got := addDecimal(nil, dec(42, 0))
	if got.Coefficient != 42 || got.Exponent != 0 {
		t.Errorf("nil + 42 = %d × 10^%d want 42 × 10^0", got.Coefficient, got.Exponent)
	}
	got = addDecimal(nil, nil)
	if got.Coefficient != 0 {
		t.Errorf("nil + nil = %d want 0", got.Coefficient)
	}
}

func TestNegateDecimal(t *testing.T) {
	got := negateDecimal(dec(42, -1))
	if got.Coefficient != -42 || got.Exponent != -1 {
		t.Errorf("got %d × 10^%d want -42 × 10^-1", got.Coefficient, got.Exponent)
	}
}

func TestAbsDecimal(t *testing.T) {
	if got := absDecimal(dec(-5, 0)); got.Coefficient != 5 {
		t.Errorf("abs(-5)=%d want 5", got.Coefficient)
	}
	if got := absDecimal(dec(5, 0)); got.Coefficient != 5 {
		t.Errorf("abs(5)=%d want 5", got.Coefficient)
	}
	if got := absDecimal(nil); got.Coefficient != 0 {
		t.Errorf("abs(nil)=%d want 0", got.Coefficient)
	}
}

func TestAddMoney_NilHandling(t *testing.T) {
	if got := addMoney(nil, nil); got != nil {
		t.Errorf("nil+nil=%v want nil", got)
	}
	m := &commonpb.Money{Amount: dec(1, 0), CurrencyCode: "USD"}
	if got := addMoney(nil, m); got != m {
		t.Errorf("nil+m should return m unchanged")
	}
}

func TestPow10(t *testing.T) {
	cases := map[int64]int64{0: 1, 1: 10, 2: 100, 3: 1000, -1: 1, -5: 1}
	for n, want := range cases {
		got, ok := pow10(n)
		if !ok || got != want {
			t.Errorf("pow10(%d)=%d,%v want %d,true", n, got, ok, want)
		}
	}
}

// --- #216: the arithmetic must keep the MAGNITUDE, never wrap ------------

// TestShockMoney_MinusTwentyPercentOnTwoHundredThousand is the reported defect.
//
// A -20% stress on a $200,000 long returned -$24,467: not merely wrong,
// SIGN-FLIPPED, so the scenario said a 20% crash makes the desk money. The
// inputs are ordinary — dec.ToProtoScaled emits exponent -8 whenever it fits and
// the shock arrives at -6 — so the int64 product 2e13 × 8e5 = 1.6e19 clears
// MaxInt64 (9.223e18) and wraps. The threshold for a -20% shock is a position of
// $115,292.15, which is a small position on this platform.
func TestShockMoney_MinusTwentyPercentOnTwoHundredThousand(t *testing.T) {
	mv := &commonpb.Money{Amount: dec(20_000_000_000_000, -8), CurrencyCode: "USD"} // $200,000
	got := ShockMoney(mv, dec(-200_000, -6))                                        // -20%
	wantValue(t, got.GetAmount(), "160000", "ShockMoney($200,000, -20%)")
}

// TestShockMoney_OverflowThresholdAndBeyond walks the wrap boundary the old
// mulDecimal sat on. $115,292.15 is the last position whose -20% product fits an
// int64 at exponent -14; everything above it wrapped, and the failure was silent
// — a plausible number, on a stress report somebody sizes a hedge from.
func TestShockMoney_OverflowThresholdAndBeyond(t *testing.T) {
	cases := []struct {
		name  string
		coeff int64 // MarketValue at exponent -8
		want  string
	}{
		{"just under the old wrap point", 11_529_215_000_000, "92233.72"},
		{"just over it", 11_529_216_000_000, "92233.728"},
		{"a $10m position", 1_000_000_000_000_000, "8000000"},
		{"a $10bn book", 1_000_000_000_000_000_000, "8000000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mv := &commonpb.Money{Amount: dec(tc.coeff, -8), CurrencyCode: "USD"}
			got := ShockMoney(mv, dec(-200_000, -6))
			wantValue(t, got.GetAmount(), tc.want, "ShockMoney")
			if got.GetAmount().GetCoefficient() < 0 {
				t.Fatalf("sign flipped: a -20%% shock on a LONG position returned %d",
					got.GetAmount().GetCoefficient())
			}
		})
	}
}

// TestSumInBaseCurrency_MixesShockedAndUnshockedWithoutWrapping covers the
// SECOND wrap, the one repairing the multiply does not remove by itself.
//
// After a shock the shocked positions carry a coarser exponent than the
// untouched ones (-13 vs -8 here). decAccum aligns to the more precise of the
// two, so an unshocked $10,000,000 (1e15 at -8) gets multiplied by 1e5 and
// leaves int64 range on its own, with no multiply anywhere in sight. Gross
// exposure is what a risk limit is checked against, so a wrapped sum here is a
// limit that passes on a book that breached it.
func TestSumInBaseCurrency_MixesShockedAndUnshockedWithoutWrapping(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	p := domain.NewPortfolio("P-216", "USD")
	p.SetAggregate(domain.AggregateUpdate{AsOf: at, BaseCurrency: "USD"})

	shocked := ShockMoney(
		&commonpb.Money{Amount: dec(20_000_000_000_000, -8), CurrencyCode: "USD"},
		dec(-200_000, -6),
	)
	p.SetPosition(domain.Position{InstrumentID: "SHOCKED", MarketValue: shocked, AsOf: at})
	p.SetPosition(domain.Position{
		InstrumentID: "UNSHOCKED",
		MarketValue:  &commonpb.Money{Amount: dec(1_000_000_000_000_000, -8), CurrencyCode: "USD"}, // $10,000,000
		AsOf:         at,
	})

	// 160,000 (shocked) + 10,000,000 = 10,160,000.
	wantValue(t, sumInBaseCurrency(p, false), "10160000", "net exposure")
	wantValue(t, sumInBaseCurrency(p, true), "10160000", "gross exposure")
}

// TestDecAccum_MatchesAddDecimalPastInt64Range pins the invariant decAccum's own
// doc claims: a sum folded through the allocation-free accumulator equals the
// same sum folded through addDecimal. It used to hold only while the int64 fast
// path did — past that addDecimal was wrong too, so the two agreed on a wrapped
// answer. The accumulator must SPILL to the exact path instead.
func TestDecAccum_MatchesAddDecimalPastInt64Range(t *testing.T) {
	terms := []*commonpb.Decimal{
		dec(4_000_000_000_000_000_000, -8), // $40bn
		dec(4_000_000_000_000_000_000, -8),
		dec(4_000_000_000_000_000_000, -8), // the running sum leaves int64 here
		dec(1, -14),                        // and a term far below it, to exercise alignment
	}
	var acc decAccum
	folded := zeroDecimal()
	for _, term := range terms {
		acc.add(term, false)
		folded = addDecimal(folded, term)
	}
	wantValue(t, acc.decimal(), "120000000000", "decAccum sum")
	wantValue(t, folded, "120000000000", "addDecimal fold")
}

// TestAbsDecimal_MinInt64IsNotNegative closes the same wrap one operator over:
// Go's -MinInt64 is MinInt64, so the old absDecimal returned a NEGATIVE absolute
// value — folded into a gross exposure that SUBTRACTS a position from the size
// of the book it is meant to enlarge.
func TestAbsDecimal_MinInt64IsNotNegative(t *testing.T) {
	got := absDecimal(dec(math.MinInt64, -8))
	if got == nil || got.GetCoefficient() < 0 {
		t.Fatalf("absDecimal(MinInt64) = %v, want a positive magnitude", got)
	}
	wantValue(t, got, "92233720368.5477581", "absDecimal(MinInt64 e-8)")
}

// TestPow10_RefusesPastInt64 is the #246 half of the bound. pow10 was an
// unbounded `for i := 0; i < n; i++` loop, so a caller-supplied exponent of
// -2000000000 executed two billion iterations PER POSITION, PER SHOCK — the risk
// engine stops answering and still reports healthy, which is the worst shape of
// failure this platform has. Beyond 10^18 there is no int64 answer to return, so
// the bound costs nothing a correct caller wanted.
func TestPow10_RefusesPastInt64(t *testing.T) {
	if _, ok := pow10(19); ok {
		t.Fatal("pow10(19) reported ok — 10^19 does not fit an int64, so the caller got a wrapped scale factor")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, ok := pow10(2_000_000_000); ok {
			t.Error("pow10(2000000000) reported ok")
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pow10(2000000000) did not return within 2s — the exponent bound is gone and a single crafted shock stalls the engine")
	}
}
