package alternatives

import (
	"math"
	"testing"
	"time"
)

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestIRRKnownCashflows(t *testing.T) {
	// -100 at t0, +121 two years later ⇒ (1+r)^2 = 1.21 ⇒ r = 10%. 2021→2023 is
	// exactly 730 days (both non-leap), so actual/365 gives exactly 2.0 years.
	flows := []CashFlow{
		{Date: day(2021, 1, 1), Amount: -100},
		{Date: day(2023, 1, 1), Amount: 121},
	}
	irr, err := IRR(flows)
	if err != nil {
		t.Fatal(err)
	}
	if !approx(irr, 0.10, 1e-4) {
		t.Fatalf("IRR: want 0.10 got %v", irr)
	}
}

func TestIRRNeedsBothSigns(t *testing.T) {
	flows := []CashFlow{{Date: day(2020, 1, 1), Amount: -100}, {Date: day(2021, 1, 1), Amount: -50}}
	if _, err := IRR(flows); err == nil {
		t.Fatal("IRR with no positive flow should error")
	}
}

func TestIRRFromPositionUsesNAVAsTerminal(t *testing.T) {
	// Call 100 at t0, NAV 110 one year later, no distributions ⇒ IRR ≈ 10%.
	// 2021→2022 is exactly 365 days (non-leap).
	p := Replay("C1", []*Event{
		ev("e1", EventCall, "100", day(2021, 1, 1)),
		ev("e2", EventNAVMark, "110", day(2022, 1, 1)),
	})
	irr, err := IRR(IRRFlows(p))
	if err != nil {
		t.Fatal(err)
	}
	if !approx(irr, 0.10, 1e-3) {
		t.Fatalf("position IRR: want ~0.10 got %v", irr)
	}
}

func TestMultiplesIdentity(t *testing.T) {
	p := Replay("C1", []*Event{
		ev("e1", EventCall, "100", day(2020, 1, 1)),
		ev("e2", EventDistribution, "60", day(2021, 1, 1)),
		ev("e3", EventNAVMark, "80", day(2022, 1, 1)),
	})
	m := ComputeMultiples(p)
	if !approx(m.DPI, 0.6, 1e-9) || !approx(m.RVPI, 0.8, 1e-9) || !approx(m.TVPI, 1.4, 1e-9) {
		t.Fatalf("multiples: %+v", m)
	}
	// TVPI = DPI + RVPI by construction.
	if !approx(m.TVPI, m.DPI+m.RVPI, 1e-9) {
		t.Fatalf("TVPI identity broken: %+v", m)
	}
}

func TestMultiplesZeroPaidIn(t *testing.T) {
	p := Replay("C1", []*Event{ev("e1", EventCommit, "1000", day(2020, 1, 1))})
	if m := ComputeMultiples(p); m.TVPI != 0 || m.DPI != 0 || m.RVPI != 0 {
		t.Fatalf("zero paid-in should give zero multiples, got %+v", m)
	}
}

func TestPMEvsBenchmark(t *testing.T) {
	// Call 100 at t0, NAV 150 at valuation — no distributions.
	p := Replay("C1", []*Event{
		ev("e1", EventCall, "100", day(2020, 1, 1)),
		ev("e2", EventNAVMark, "150", day(2022, 1, 1)),
	})
	val := day(2022, 1, 1)

	// Flat benchmark ⇒ no scaling ⇒ PME = (NAV) / contribution = 1.5 (beat a flat market).
	flat := func(time.Time) float64 { return 100 }
	pme, err := PME(p, val, flat)
	if err != nil {
		t.Fatal(err)
	}
	if !approx(pme, 1.5, 1e-9) {
		t.Fatalf("flat-benchmark PME: want 1.5 got %v", pme)
	}

	// Benchmark doubled over the period ⇒ contribution future-values to 200 ⇒
	// PME = 150/200 = 0.75 (the index would have beaten the fund).
	doubling := func(d time.Time) float64 {
		if d.Equal(day(2020, 1, 1)) {
			return 100
		}
		return 200
	}
	pme, err = PME(p, val, doubling)
	if err != nil {
		t.Fatal(err)
	}
	if !approx(pme, 0.75, 1e-9) {
		t.Fatalf("doubling-benchmark PME: want 0.75 got %v", pme)
	}
}
