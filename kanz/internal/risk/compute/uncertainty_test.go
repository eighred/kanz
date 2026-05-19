package compute_test

import (
	"math"
	"testing"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/kanz-eng/kanz/internal/risk/compute"
	"github.com/kanz-eng/kanz/internal/risk/domain"
)

// uncertaintyEpsilon is the tolerance for sqrt round-trip
// comparisons — generous enough for the 6-decimal-place propagation
// precision (uncertaintyExp = -6), tight enough to catch real bugs.
const uncertaintyEpsilon = 1e-4

func decValue(d *commonpb.Decimal) float64 {
	if d == nil {
		return 0
	}
	return float64(d.Coefficient) * math.Pow10(int(d.Exponent))
}

func TestPropagateSumIndependent_ClassicTriple(t *testing.T) {
	// √(3² + 4²) = √25 = 5
	got := compute.PropagateSumIndependent(
		&commonpb.Decimal{Coefficient: 3, Exponent: 0},
		&commonpb.Decimal{Coefficient: 4, Exponent: 0},
	)
	if math.Abs(decValue(got)-5.0) > uncertaintyEpsilon {
		t.Errorf("propagated = %v want 5.0", decValue(got))
	}
}

func TestPropagateSumIndependent_SqrtTwo(t *testing.T) {
	// √(1² + 1²) = √2 ≈ 1.414
	got := compute.PropagateSumIndependent(
		&commonpb.Decimal{Coefficient: 1, Exponent: 0},
		&commonpb.Decimal{Coefficient: 1, Exponent: 0},
	)
	if math.Abs(decValue(got)-math.Sqrt(2)) > uncertaintyEpsilon {
		t.Errorf("propagated = %v want √2 ≈ %v", decValue(got), math.Sqrt(2))
	}
}

func TestPropagateSumIndependent_AllNilReturnsNil(t *testing.T) {
	if got := compute.PropagateSumIndependent(); got != nil {
		t.Errorf("got %v want nil (no inputs ⇒ no propagation signal)", got)
	}
	if got := compute.PropagateSumIndependent(nil, nil); got != nil {
		t.Errorf("got %v want nil (all-nil ⇒ no propagation signal)", got)
	}
}

func TestPropagateSumIndependent_NilSkipped(t *testing.T) {
	// {nil, 3, nil, 4} should still compute √25 = 5
	got := compute.PropagateSumIndependent(
		nil,
		&commonpb.Decimal{Coefficient: 3, Exponent: 0},
		nil,
		&commonpb.Decimal{Coefficient: 4, Exponent: 0},
	)
	if math.Abs(decValue(got)-5.0) > uncertaintyEpsilon {
		t.Errorf("propagated = %v want 5.0", decValue(got))
	}
}

func TestPropagateSumPerfectlyCorrelated_LinearSum(t *testing.T) {
	// |3| + |4| + |-1| = 8
	got := compute.PropagateSumPerfectlyCorrelated(
		&commonpb.Decimal{Coefficient: 3, Exponent: 0},
		&commonpb.Decimal{Coefficient: 4, Exponent: 0},
		&commonpb.Decimal{Coefficient: -1, Exponent: 0}, // abs applied
	)
	if math.Abs(decValue(got)-8.0) > uncertaintyEpsilon {
		t.Errorf("propagated = %v want 8.0", decValue(got))
	}
}

func TestPropagateSumPerfectlyCorrelated_AllNilReturnsNil(t *testing.T) {
	if got := compute.PropagateSumPerfectlyCorrelated(nil, nil); got != nil {
		t.Errorf("got %v want nil", got)
	}
}

func TestPropagateScalar_ExactMultiplication(t *testing.T) {
	// |2| × |3| = 6
	got := compute.PropagateScalar(
		&commonpb.Decimal{Coefficient: 2, Exponent: 0},
		&commonpb.Decimal{Coefficient: 3, Exponent: 0},
	)
	if math.Abs(decValue(got)-6.0) > uncertaintyEpsilon {
		t.Errorf("propagated = %v want 6.0", decValue(got))
	}
}

func TestPropagateScalar_NegativeScalarTakesAbsolute(t *testing.T) {
	// |-0.5| × |10| = 5
	got := compute.PropagateScalar(
		&commonpb.Decimal{Coefficient: -5, Exponent: -1},
		&commonpb.Decimal{Coefficient: 10, Exponent: 0},
	)
	if math.Abs(decValue(got)-5.0) > uncertaintyEpsilon {
		t.Errorf("propagated = %v want 5.0", decValue(got))
	}
}

func TestPropagateScalar_NilReturnsNil(t *testing.T) {
	if got := compute.PropagateScalar(nil, &commonpb.Decimal{Coefficient: 1}); got != nil {
		t.Errorf("nil scalar ⇒ nil got %v", got)
	}
	if got := compute.PropagateScalar(&commonpb.Decimal{Coefficient: 1}, nil); got != nil {
		t.Errorf("nil uncertainty ⇒ nil got %v", got)
	}
}

// --- Integration with the baseline measures (RISK-07 + RISK-08) -------

// mkMoneyUnc is a helper for tests that need to construct a Position
// with both MarketValue and MarketValueUncertainty.
func mkMoneyUnc(amount int64, exp int32, ccy string) *commonpb.Money {
	return &commonpb.Money{
		Amount:       &commonpb.Decimal{Coefficient: amount, Exponent: exp},
		CurrencyCode: ccy,
	}
}

func TestGrossExposure_UncertaintyPropagatesIndependentSum(t *testing.T) {
	// Two positions: σ_1 = 3, σ_2 = 4 ⇒ propagated σ = √(9+16) = 5
	p := makePortfolio("PORT-1", "USD",
		domain.Position{
			InstrumentID:           "A",
			MarketValue:            mkMoneyUnc(100, 0, "USD"),
			MarketValueUncertainty: mkMoneyUnc(3, 0, "USD"),
			AsOf:                   baseTime,
		},
		domain.Position{
			InstrumentID:           "B",
			MarketValue:            mkMoneyUnc(200, 0, "USD"),
			MarketValueUncertainty: mkMoneyUnc(4, 0, "USD"),
			AsOf:                   baseTime,
		},
	)
	got := compute.GrossExposure(p)
	if math.Abs(decValue(got.UncertaintyAbs)-5.0) > uncertaintyEpsilon {
		t.Errorf("UncertaintyAbs = %v want 5.0", decValue(got.UncertaintyAbs))
	}
}

func TestGrossExposure_NoUncertaintyInputsLeavesUncertaintyNil(t *testing.T) {
	// No position carries an uncertainty — UncertaintyAbs must stay
	// nil so the caller sees "no uncertainty propagated", not a
	// spurious zero.
	p := makePortfolio("PORT-1", "USD",
		domain.Position{
			InstrumentID: "A",
			MarketValue:  mkMoneyUnc(100, 0, "USD"),
			AsOf:         baseTime,
		},
	)
	got := compute.GrossExposure(p)
	if got.UncertaintyAbs != nil {
		t.Errorf("UncertaintyAbs = %v want nil", got.UncertaintyAbs)
	}
}

func TestVaR99_UncertaintyScaledBy1pct(t *testing.T) {
	// σ_gross = 100; σ_VaR = 0.01 × 100 = 1
	p := makePortfolio("PORT-1", "USD",
		domain.Position{
			InstrumentID:           "A",
			MarketValue:            mkMoneyUnc(10000, 0, "USD"),
			MarketValueUncertainty: mkMoneyUnc(100, 0, "USD"),
			AsOf:                   baseTime,
		},
	)
	got := compute.VaR99(p)
	if math.Abs(decValue(got.UncertaintyAbs)-1.0) > uncertaintyEpsilon {
		t.Errorf("VaR99 UncertaintyAbs = %v want 1.0 (= 0.01 × 100)", decValue(got.UncertaintyAbs))
	}
}

func TestDelta_UncertaintyMatchesNet(t *testing.T) {
	p := makePortfolio("PORT-1", "USD",
		domain.Position{
			InstrumentID:           "A",
			MarketValue:            mkMoneyUnc(100, 0, "USD"),
			MarketValueUncertainty: mkMoneyUnc(7, 0, "USD"),
			AsOf:                   baseTime,
		},
	)
	d := compute.Delta(p)
	n := compute.NetExposure(p)
	if decValue(d.UncertaintyAbs) != decValue(n.UncertaintyAbs) {
		t.Errorf("Delta σ=%v want NetExposure σ=%v", decValue(d.UncertaintyAbs), decValue(n.UncertaintyAbs))
	}
}

func TestSumUncertaintyInBaseCurrency_SkipsForeignCurrencyUncertainty(t *testing.T) {
	// MarketValue + Uncertainty in EUR — must be skipped (no FX
	// conversion in compute, same rule as RISK-06).
	p := makePortfolio("PORT-1", "USD",
		domain.Position{
			InstrumentID:           "USD-POS",
			MarketValue:            mkMoneyUnc(100, 0, "USD"),
			MarketValueUncertainty: mkMoneyUnc(10, 0, "USD"),
			AsOf:                   baseTime,
		},
		domain.Position{
			InstrumentID:           "EUR-POS",
			MarketValue:            mkMoneyUnc(50, 0, "EUR"),
			MarketValueUncertainty: mkMoneyUnc(999, 0, "EUR"), // huge, must be excluded
			AsOf:                   baseTime,
		},
	)
	got := compute.GrossExposure(p)
	// Only USD-POS uncertainty (10) — propagated alone = √100 = 10.
	if math.Abs(decValue(got.UncertaintyAbs)-10.0) > uncertaintyEpsilon {
		t.Errorf("UncertaintyAbs = %v want 10.0 (EUR uncertainty must not contribute)", decValue(got.UncertaintyAbs))
	}
}
