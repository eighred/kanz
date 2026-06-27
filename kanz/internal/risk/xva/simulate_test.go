package xva

import "testing"

// longForward is a long forward on factor S struck at-the-money — the simplest
// monotone exposure generator.
func longForward(spot, vol float64) (NettingSet, []RiskFactor) {
	set := NettingSet{CounterpartyID: "CP", Trades: []Pricer{LinearTrade{FactorID: "S", Strike: spot, Notional: 1}}}
	factors := []RiskFactor{{ID: "S", Spot: spot, Drift: 0, Vol: vol, Kind: Lognormal}}
	return set, factors
}

func TestPFE_MonotoneInHorizon(t *testing.T) {
	set, factors := longForward(100, 0.30)
	prof := Config{Paths: 8000, Seed: 5}.Simulate(set, factors, nil, []float64{0.25, 0.5, 1, 2, 5})
	for k := 1; k < len(prof.PFE); k++ {
		if prof.PFE[k] < prof.PFE[k-1] {
			t.Fatalf("PFE must grow with horizon: PFE[%d]=%.4f < PFE[%d]=%.4f", k, prof.PFE[k], k-1, prof.PFE[k-1])
		}
	}
}

func TestPFE_MonotoneInVol(t *testing.T) {
	grid := []float64{1}
	lo, loF := longForward(100, 0.20)
	hi, hiF := longForward(100, 0.40)
	pLo := Config{Paths: 8000, Seed: 9}.Simulate(lo, loF, nil, grid)
	pHi := Config{Paths: 8000, Seed: 9}.Simulate(hi, hiF, nil, grid)
	if pHi.PFE[0] <= pLo.PFE[0] {
		t.Fatalf("higher vol must raise PFE: %.4f !> %.4f", pHi.PFE[0], pLo.PFE[0])
	}
	if pHi.EE[0] <= pLo.EE[0] {
		t.Fatalf("higher vol must raise EE: %.4f !> %.4f", pHi.EE[0], pLo.EE[0])
	}
}

func TestNettingReducesExposure(t *testing.T) {
	spot := 100.0
	factors := []RiskFactor{{ID: "S", Spot: spot, Drift: 0, Vol: 0.30, Kind: Lognormal}}
	grid := []float64{1, 2, 3}
	cfg := Config{Paths: 8000, Seed: 3}

	long := NettingSet{Trades: []Pricer{LinearTrade{FactorID: "S", Strike: spot, Notional: 1}}}
	short := NettingSet{Trades: []Pricer{LinearTrade{FactorID: "S", Strike: spot, Notional: -1}}}
	both := NettingSet{Trades: []Pricer{
		LinearTrade{FactorID: "S", Strike: spot, Notional: 1},
		LinearTrade{FactorID: "S", Strike: spot, Notional: -1},
	}}

	epeLong := cfg.Simulate(long, factors, nil, grid).EPE()
	epeShort := cfg.Simulate(short, factors, nil, grid).EPE()
	epeBoth := cfg.Simulate(both, factors, nil, grid).EPE()

	if epeLong <= 0 || epeShort <= 0 {
		t.Fatalf("standalone exposures must be positive: long=%.4f short=%.4f", epeLong, epeShort)
	}
	// The offsetting trades net to zero value ⇒ the netted exposure is far below
	// the sum of the standalone exposures (the netting benefit).
	if epeBoth >= epeLong+epeShort {
		t.Fatalf("netting must reduce exposure below the gross sum: both=%.4f gross=%.4f", epeBoth, epeLong+epeShort)
	}
	if epeBoth > 1e-9 {
		t.Fatalf("perfectly offsetting trades should net to ~0 exposure, got %.6f", epeBoth)
	}
}

func TestCSA_ReducesExposure(t *testing.T) {
	set, factors := longForward(100, 0.30)
	grid := []float64{1, 2, 3}
	cfg := Config{Paths: 6000, Seed: 7}
	uncollat := cfg.Simulate(set, factors, nil, grid).EPE()

	set.CSA = &CSA{Threshold: 5} // counterparty posts above a 5 threshold
	collat := cfg.Simulate(set, factors, nil, grid).EPE()
	if collat >= uncollat {
		t.Fatalf("a CSA must reduce expected exposure: %.4f !< %.4f", collat, uncollat)
	}
}
