package performance

import (
	"math"
	"testing"
	"time"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %.8f want %.8f (Δ %.2e)", name, got, want, math.Abs(got-want))
	}
}

func TestTimeWeightedReturn_NeutralToFlows(t *testing.T) {
	// Period 1: 100 → 110 (no flow), r1 = +10%.
	// Period 2: a +50 contribution at the start, 160 → 176, r2 = 176/160−1 = +10%.
	// TWR links 1.1·1.1−1 = 0.21 — the flow does NOT distort manager return.
	periods := []SubPeriod{
		{BeginValue: 100, Flow: 0, EndValue: 110},
		{BeginValue: 110, Flow: 50, EndValue: 176},
	}
	near(t, "TWR", TimeWeightedReturn(periods), 0.21, 1e-12)
}

func TestModifiedDietz_FlowWeighted(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)
	mid := start.Add(end.Sub(start) / 2)
	// begin 100, end 176, +50 flow at mid-period. gain = 176−100−50 = 26;
	// avg capital = 100 + 0.5·50 = 125; MWR = 26/125 = 0.208.
	p := Period{Start: start, End: end, BeginValue: 100, EndValue: 176, Flows: []Flow{{Time: mid, Amount: 50}}}
	near(t, "Modified Dietz", p.ModifiedDietz(), 0.208, 1e-9)
}

func TestIRR_NoFlows(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(2, 0, 0) // 2 years
	// 100·(1+r)^2 = 121 ⇒ r ≈ 10% (the window is 731 actual days — 2024 is a leap
	// year — so the ACT/365-annualized rate is fractionally under 0.10).
	p := Period{Start: start, End: end, BeginValue: 100, EndValue: 121}
	near(t, "IRR", p.IRR(), 0.10, 1e-3)
}

func TestIRR_WithFlowReflectsTiming(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)
	// A contribution late in the period that earns little ⇒ MWR below the TWR a
	// flow-neutral measure would show. Just assert IRR is finite and below the
	// gross 50% naive (176−100)/100 a flow-blind calc would claim.
	p := Period{Start: start, End: end, BeginValue: 100, EndValue: 176, Flows: []Flow{{Time: end.AddDate(0, -1, 0), Amount: 50}}}
	irr := p.IRR()
	if math.IsNaN(irr) || irr <= 0 || irr >= 0.5 {
		t.Fatalf("IRR should be a finite positive rate well under 50%%, got %.4f", irr)
	}
}

func TestAnnualize(t *testing.T) {
	near(t, "annualize 21%/2y", Annualize(0.21, 2), 0.10, 1e-9)
	near(t, "annualize ≤0 window", Annualize(0.05, 0), 0.05, 0)
}

func TestSubPeriod_DegenerateBaseIsZero(t *testing.T) {
	near(t, "zero base", SubPeriod{BeginValue: 0, Flow: 0, EndValue: 10}.Return(), 0, 0)
}
