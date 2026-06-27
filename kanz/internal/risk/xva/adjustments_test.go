package xva

import (
	"math"
	"testing"
)

// flatProfile is a constant EE/NEE exposure profile on a yearly grid.
func flatProfile(years int, ee, nee float64) ExposureProfile {
	p := ExposureProfile{}
	for k := 1; k <= years; k++ {
		p.Times = append(p.Times, float64(k))
		p.EE = append(p.EE, ee)
		p.NEE = append(p.NEE, nee)
		p.PFE = append(p.PFE, ee)
	}
	return p
}

// TestCVA_MatchesAnalytic: with a flat EE profile, a flat hazard, and a zero
// discount rate, the discrete CVA telescopes to the closed form
// LGD·EE·(1−e^{−hT}).
func TestCVA_MatchesAnalytic(t *testing.T) {
	const ee, h, recovery, years = 1_000_000.0, 0.02, 0.4, 5
	a := Adjustments{
		Profile:      flatProfile(years, ee, 0),
		Counterparty: FlatHazard(h, recovery),
		DiscountRate: 0,
	}
	got := a.CVA()
	want := (1 - recovery) * ee * (1 - math.Exp(-h*years))
	if math.Abs(got-want) > 1e-6*want {
		t.Fatalf("CVA %.4f must match analytic %.4f", got, want)
	}
	// A positive discount rate lowers CVA (future losses discounted).
	disc := Adjustments{Profile: flatProfile(years, ee, 0), Counterparty: FlatHazard(h, recovery), DiscountRate: 0.05}
	if disc.CVA() >= got {
		t.Fatalf("discounting must lower CVA: %.4f !< %.4f", disc.CVA(), got)
	}
}

// TestDVA_FVA: DVA is driven by the negative exposure and our own curve; FVA by
// the funding spread. Both positive when their drivers are.
func TestDVA_FVA(t *testing.T) {
	a := Adjustments{
		Profile:       flatProfile(5, 800_000, 300_000),
		Counterparty:  FlatHazard(0.02, 0.4),
		Own:           FlatHazard(0.015, 0.4),
		DiscountRate:  0.03,
		FundingSpread: 0.01,
	}
	if a.DVA() <= 0 {
		t.Fatalf("DVA must be positive with a negative-exposure profile + own curve, got %.4f", a.DVA())
	}
	if a.FVA() <= 0 {
		t.Fatalf("FVA must be positive with a funding spread, got %.4f", a.FVA())
	}
	// No own curve ⇒ no DVA.
	noOwn := a
	noOwn.Own = CreditCurve{}
	if noOwn.DVA() != 0 {
		t.Fatalf("DVA without an own credit curve must be 0, got %.4f", noOwn.DVA())
	}
	// BCVA = CVA − DVA.
	if math.Abs(a.BCVA()-(a.CVA()-a.DVA())) > 1e-9 {
		t.Fatal("BCVA must equal CVA − DVA")
	}
}

// TestCreditStress_RaisesCVA: widening spreads raises CVA; a jump-to-default
// crystallizes the near-term loss.
func TestCreditStress_RaisesCVA(t *testing.T) {
	a := Adjustments{Profile: flatProfile(5, 1_000_000, 0), Counterparty: FromCDS(0.01, 0.4), DiscountRate: 0.02}
	base := a.CVA()
	widened := CreditStress{HazardMult: 3}.ApplyTo(a).CVA()
	if widened <= base {
		t.Fatalf("spread widening must raise CVA: %.2f !> %.2f", widened, base)
	}
	jtd := CreditStress{JumpToDefault: true}.ApplyTo(a).CVA()
	if jtd <= base {
		t.Fatalf("jump-to-default must raise CVA above base, got %.2f vs %.2f", jtd, base)
	}
}
