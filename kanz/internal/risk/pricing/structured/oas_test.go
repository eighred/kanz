package structured

import (
	"math"
	"testing"

	"github.com/kanz-eng/kanz/internal/risk/pricing/curve"
)

func testEnv(t *testing.T) RateEnv {
	t.Helper()
	c, err := curve.NewZeroCurve([]float64{1, 5, 10, 30}, []float64{0.03, 0.035, 0.04, 0.045}, curve.Continuous, curve.LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	return RateEnv{Curve: c, RefTenor: 10}
}

// TestOAS_RoundTripsToPrice: pricing a tranche at a known spread, then solving
// the OAS to that price, recovers the spread.
func TestOAS_RoundTripsToPrice(t *testing.T) {
	d := testDeal()
	env := testEnv(t)
	pm := ConstantCPR{CPR: 0.08, CDR: 0.01, Sev: 0.35} // rate-independent ⇒ clean round-trip
	const spread = 0.012

	price := PriceTranche(d.Project(pm, env), 0, env, spread)
	oas := OAS(d, pm, env, 0, price)
	if math.Abs(oas-spread) > 1e-5 {
		t.Fatalf("OAS round-trip: got %.6f want %.6f", oas, spread)
	}
	// Repricing at the solved OAS recovers the price.
	if rp := PriceTranche(d.Project(pm, env), 0, env, oas); math.Abs(rp-price) > 1e-4 {
		t.Fatalf("reprice at OAS %.6f != price %.4f (got %.4f)", oas, price, rp)
	}
}

// TestEffectiveDuration_SignAndMonotonicity: effective duration is positive
// (price falls as rates rise) and the ±shift prices are monotone in rates.
func TestEffectiveDuration_SignAndMonotonicity(t *testing.T) {
	d := testDeal()
	env := testEnv(t)
	pm := Behavioral{Base: 0.05, Max: 0.45, Steepness: 40, CDR: 0.01, Sev: 0.35}
	const oas = 0.01

	p0 := PriceTranche(d.Project(pm, env), 0, env, oas)
	pUp := PriceTranche(d.Project(pm, shiftEnv(env, 100)), 0, shiftEnv(env, 100), oas)
	pDown := PriceTranche(d.Project(pm, shiftEnv(env, -100)), 0, shiftEnv(env, -100), oas)
	if !(pUp < p0 && p0 < pDown) {
		t.Fatalf("price must decrease as rates rise: pUp=%.2f p0=%.2f pDown=%.2f", pUp, p0, pDown)
	}

	effDur, _ := EffectiveRisk(d, pm, env, 0, oas, 25)
	if effDur <= 0 {
		t.Fatalf("effective duration must be positive, got %.4f", effDur)
	}
}

// TestEffectiveConvexity_PrepayHurtsConvexity: a rate-responsive (behavioral)
// prepay model gives lower effective convexity than a rate-insensitive one — the
// prepayment option's negative convexity.
func TestEffectiveConvexity_PrepayHurtsConvexity(t *testing.T) {
	d := testDeal()
	env := testEnv(t)
	const oas = 0.01
	_, convConst := EffectiveRisk(d, ConstantCPR{CPR: 0.10, CDR: 0.01, Sev: 0.35}, env, 0, oas, 25)
	_, convBehav := EffectiveRisk(d, Behavioral{Base: 0.05, Max: 0.60, Steepness: 50, CDR: 0.01, Sev: 0.35}, env, 0, oas, 25)
	if convBehav >= convConst {
		t.Fatalf("the prepay option must lower convexity: behavioral=%.2f !< constant=%.2f", convBehav, convConst)
	}
}
