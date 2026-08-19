package structured

import (
	"errors"
	"math"
	"testing"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
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

	price := priced(t, d.Project(pm, env), 0, env, spread)
	oas, err := OAS(d, pm, env, 0, price)
	if err != nil {
		t.Fatalf("OAS: %v", err)
	}
	if math.Abs(oas-spread) > 1e-5 {
		t.Fatalf("OAS round-trip: got %.6f want %.6f", oas, spread)
	}
	// Repricing at the solved OAS recovers the price.
	if rp := priced(t, d.Project(pm, env), 0, env, oas); math.Abs(rp-price) > 1e-4 {
		t.Fatalf("reprice at OAS %.6f != price %.4f (got %.4f)", oas, price, rp)
	}
}

func priced(t *testing.T, proj Projection, idx int, env RateEnv, spread float64) float64 {
	t.Helper()
	pv, err := PriceTranche(proj, idx, env, spread)
	if err != nil {
		t.Fatalf("PriceTranche(%d): %v", idx, err)
	}
	return pv
}

func effRisk(t *testing.T, d Deal, pm PrepayModel, env RateEnv, idx int, oas, bp float64) (float64, float64) {
	t.Helper()
	dur, conv, err := EffectiveRisk(d, pm, env, idx, oas, bp)
	if err != nil {
		t.Fatalf("EffectiveRisk: %v", err)
	}
	return dur, conv
}

// TestEffectiveDuration_SignAndMonotonicity: effective duration is positive
// (price falls as rates rise) and the ±shift prices are monotone in rates.
func TestEffectiveDuration_SignAndMonotonicity(t *testing.T) {
	d := testDeal()
	env := testEnv(t)
	pm := Behavioral{Base: 0.05, Max: 0.45, Steepness: 40, CDR: 0.01, Sev: 0.35}
	const oas = 0.01

	p0 := priced(t, d.Project(pm, env), 0, env, oas)
	pUp := priced(t, d.Project(pm, shiftEnv(env, 100)), 0, shiftEnv(env, 100), oas)
	pDown := priced(t, d.Project(pm, shiftEnv(env, -100)), 0, shiftEnv(env, -100), oas)
	if !(pUp < p0 && p0 < pDown) {
		t.Fatalf("price must decrease as rates rise: pUp=%.2f p0=%.2f pDown=%.2f", pUp, p0, pDown)
	}

	effDur, _ := effRisk(t, d, pm, env, 0, oas, 25)
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
	_, convConst := effRisk(t, d, ConstantCPR{CPR: 0.10, CDR: 0.01, Sev: 0.35}, env, 0, oas, 25)
	_, convBehav := effRisk(t, d, Behavioral{Base: 0.05, Max: 0.60, Steepness: 50, CDR: 0.01, Sev: 0.35}, env, 0, oas, 25)
	if convBehav >= convConst {
		t.Fatalf("the prepay option must lower convexity: behavioral=%.2f !< constant=%.2f", convBehav, convConst)
	}
}

// EVERY WAY OF FAILING TO PRICE A TRANCHE MUST REFUSE, NOT ANSWER ZERO (#572).
//
// This is the shape #565 settled on one floor up: frtb.Charge returns
// ErrUncovered for a risk class its table does not cover, because a class
// contributing zero capital and a book carrying none produce the same signed
// filing. Here the same two things are an MBS tranche with no discount curve and
// an MBS tranche that genuinely does not move with rates — and the second does
// not exist.
//
// Each case asserts the ERROR, not just a non-zero value, because the pre-#572
// code returned (0, 0) with no error from every one of them and a test on the
// value alone would still pass against it.
func TestEffectiveRisk_RefusesWhatItCannotPrice(t *testing.T) {
	d := testDeal()
	env := testEnv(t)
	pm := ConstantCPR{CPR: 0.08, CDR: 0.01, Sev: 0.35}

	cases := []struct {
		name string
		env  RateEnv
		idx  int
		bp   float64
	}{
		// The state a StructuredSpec assembled without a curve arrives in — and
		// the one no provider exists to get right yet (#572).
		{"no discount curve", RateEnv{RefTenor: 10}, 0, 25},
		{"tranche the deal does not have", env, 7, 25},
		{"negative tranche index", env, -1, 25},
		{"zero bump size", env, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dur, conv, err := EffectiveRisk(d, pm, tc.env, tc.idx, 0.01, tc.bp)
			if !errors.Is(err, ErrUnpriceable) {
				t.Fatalf("EffectiveRisk returned (%v, %v, %v), want ErrUnpriceable — a duration of "+
					"zero is a claim that this tranche does not move when rates move", dur, conv, err)
			}
		})
	}
}

// PriceTranche is the layer both of EffectiveRisk's arms run through, so its own
// refusal is pinned separately: an OAS solve calls it two hundred times inside a
// bisection that cannot carry an error out, and OAS probes it once up front for
// exactly that reason.
func TestPriceTrancheAndOAS_RefuseAnEnvironmentWithNoCurve(t *testing.T) {
	d := testDeal()
	proj := d.Project(ConstantCPR{CPR: 0.08}, RateEnv{})
	if pv, err := PriceTranche(proj, 0, RateEnv{RefTenor: 10}, 0.01); !errors.Is(err, ErrUnpriceable) {
		t.Errorf("PriceTranche with no curve = (%v, %v), want ErrUnpriceable", pv, err)
	}
	if oas, err := OAS(d, ConstantCPR{CPR: 0.08}, RateEnv{RefTenor: 10}, 0, 800_000); !errors.Is(err, ErrUnpriceable) {
		t.Errorf("OAS with no curve = (%v, %v), want ErrUnpriceable — bisection against a "+
			"price function stuck at zero converges on a spread that means nothing", oas, err)
	}
}
