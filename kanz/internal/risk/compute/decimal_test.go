package compute

import (
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

func dec(c int64, e int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: c, Exponent: e}
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
	cases := map[int32]int64{0: 1, 1: 10, 2: 100, 3: 1000, -1: 1, -5: 1}
	for n, want := range cases {
		if got := pow10(n); got != want {
			t.Errorf("pow10(%d)=%d want %d", n, got, want)
		}
	}
}
