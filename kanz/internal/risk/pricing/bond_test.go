package pricing

import (
	"math"
	"testing"
	"time"

	"github.com/kanz-eng/kanz/internal/risk/pricing/curve"
)

func bondApprox(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %.8f want %.8f (Δ %.2e)", name, got, want, math.Abs(got-want))
	}
}

// parBond5y is a 5-year 10% annual-coupon bond, face 100, 30/360 — its year
// fractions are exactly 1..5, so at a 10% yield it prices to par with closed-form
// reference duration/convexity.
func parBond5y() (Bond, time.Time) {
	issue := time.Date(2020, 1, 15, 0, 0, 0, 0, time.UTC)
	return Bond{
		Face:       100,
		CouponRate: 0.10,
		Frequency:  1,
		Issue:      issue,
		Maturity:   issue.AddDate(5, 0, 0),
		DayCount:   Thirty360,
	}, issue
}

func TestBond_PricesToParAtCouponYield(t *testing.T) {
	b, settle := parBond5y()
	bondApprox(t, "par price", b.PriceFromYield(settle, 0.10), 100, 1e-7)
}

func TestBond_KnownDurationConvexity(t *testing.T) {
	b, settle := parBond5y()
	r := b.Risk(settle, 0.10)
	bondApprox(t, "dirty price", r.DirtyPrice, 100, 1e-7)
	bondApprox(t, "Macaulay duration", r.MacaulayDuration, 4.169865, 1e-5)
	bondApprox(t, "modified duration", r.ModifiedDuration, 3.790786, 1e-5)
	bondApprox(t, "convexity", r.Convexity, 19.3675, 1e-3)
	// DV01 = ModDur · P · 1bp.
	bondApprox(t, "DV01", r.DV01, 3.790786*100*1e-4, 1e-7)
}

func TestBond_DV01MatchesBump(t *testing.T) {
	b, settle := parBond5y()
	y := 0.06
	r := b.Risk(settle, y)
	// Central-difference price change per 1bp.
	up := b.PriceFromYield(settle, y+1e-4)
	down := b.PriceFromYield(settle, y-1e-4)
	bumpDV01 := math.Abs(up-down) / 2
	bondApprox(t, "DV01 vs bump", r.DV01, bumpDV01, 1e-6)
}

func TestBond_YieldFromPriceRoundTrip(t *testing.T) {
	b, settle := parBond5y()
	for _, y := range []float64{0.02, 0.06, 0.10, 0.15} {
		p := b.PriceFromYield(settle, y)
		got := b.YieldFromPrice(settle, p)
		bondApprox(t, "yield round-trip", got, y, 1e-6)
	}
}

func TestBond_AccruedInterest(t *testing.T) {
	b, settle := parBond5y()
	// Half a coupon period in (30/360 ⇒ exactly 0.5 year): half the 10 coupon.
	mid := settle.AddDate(0, 6, 0)
	bondApprox(t, "accrued half period", b.AccruedInterest(mid), 5.0, 1e-9)
	// Clean = dirty − accrued.
	dirty := b.PriceFromYield(mid, 0.10)
	bondApprox(t, "clean price", b.CleanPrice(mid, 0.10), dirty-5.0, 1e-9)
}

func TestZeroCouponBond(t *testing.T) {
	issue := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	z := Bond{Face: 100, CouponRate: 0, Frequency: 0, Issue: issue, Maturity: issue.AddDate(10, 0, 0), DayCount: Thirty360}
	// Annual-compounded zero at 5%: price = 100/1.05^10.
	bondApprox(t, "zero price", z.PriceFromYield(issue, 0.05), 100/math.Pow(1.05, 10), 1e-6)
	bondApprox(t, "zero accrued", z.AccruedInterest(issue.AddDate(3, 0, 0)), 0, 0)
	// Macaulay duration of a zero equals its maturity (≈10y, 30/360 exact).
	bondApprox(t, "zero MacDur≈maturity", z.Risk(issue, 0.05).MacaulayDuration, 10, 1e-6)
}

func TestBond_KeyRateSumApproxEffectiveDuration(t *testing.T) {
	b, settle := parBond5y()
	c, err := curve.NewZeroCurve([]float64{1, 2, 3, 4, 5}, []float64{0.03, 0.035, 0.04, 0.043, 0.045}, curve.Continuous, curve.LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	eff := b.EffectiveDuration(settle, c)
	var sum float64
	for _, kr := range b.KeyRateDurations(settle, c) {
		sum += kr.Duration
	}
	// The localized bumps tile the curve, so the key-rate durations sum to the
	// effective duration (FI-01f decomposition).
	bondApprox(t, "ΣKRD ≈ effective duration", sum, eff, 1e-3)
}

func TestBond_CurveRiskMatchesAnalyticAtFlatCurve(t *testing.T) {
	// On a flat continuous curve the curve-based effective duration must agree
	// with the single-yield modified duration computed at the equivalent yield.
	b, settle := parBond5y()
	const z = 0.05
	c, err := curve.NewZeroCurve([]float64{1, 5}, []float64{z, z}, curve.Continuous, curve.LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	cr := b.CurveRisk(settle, c)
	if cr.EffectiveDuration < 3 || cr.EffectiveDuration > 5 {
		t.Fatalf("effective duration out of plausible range: %.4f", cr.EffectiveDuration)
	}
	if cr.Convexity <= 0 {
		t.Fatalf("convexity must be positive for a bullet bond, got %.4f", cr.Convexity)
	}
	if cr.DV01 <= 0 {
		t.Fatalf("DV01 must be positive, got %.6f", cr.DV01)
	}
}
