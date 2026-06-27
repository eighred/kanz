package xva

import (
	"math"
	"testing"
)

// TestSACCR_EquityWorkedExample reproduces a hand-derived CRE52 computation for a
// single at-the-money equity forward (single-name):
//
//	MF = √min(1,1) = 1 ; D = δ·d·MF = 1·100·1 = 100
//	AddOn(entity) = SF·|D| = 0.32·100 = 32
//	single entity ⇒ AddOn(equity) = √((ρ·32)² + (1−ρ²)·32²) = 32
//	V−C = 0 ⇒ multiplier = 1 ; PFE = 32 ; RC = 0
//	EAD = α·(RC+PFE) = 1.4·32 = 44.8
func TestSACCR_EquityWorkedExample(t *testing.T) {
	ns := SACCRNettingSet{
		Trades: []SACCRTrade{{
			AssetClass: Equity, HedgingSet: "ACME",
			AdjustedNotional: 100, Delta: 1, MaturityYears: 1,
			SupervisoryFactor: 0.32, Correlation: 0.5,
		}},
	}
	if ead := ns.EAD(); math.Abs(ead-44.8) > 1e-6 {
		t.Fatalf("EAD: got %.6f want 44.8", ead)
	}
}

// TestSACCR_MultiplierBelowOne: a negative net MtM (out-of-the-money) pulls the
// multiplier below 1 via the CRE52 exponential. Hand-derived:
//
//	V−C = −20, AddOn = 32
//	multiplier = 0.05 + 0.95·exp(−20/(2·0.95·32)) = 0.733688
//	PFE = 0.733688·32 = 23.478 ; RC = 0 ; EAD = 1.4·23.478 = 32.869
func TestSACCR_MultiplierBelowOne(t *testing.T) {
	ns := SACCRNettingSet{
		NetMtM: -20,
		Trades: []SACCRTrade{{
			AssetClass: Equity, HedgingSet: "ACME",
			AdjustedNotional: 100, Delta: 1, MaturityYears: 1,
			SupervisoryFactor: 0.32, Correlation: 0.5,
		}},
	}
	if ead := ns.EAD(); math.Abs(ead-32.869) > 0.01 {
		t.Fatalf("EAD with V<0: got %.4f want ≈32.869", ead)
	}
}

// TestSACCR_NettingWithinHedgingSet: a long and a short in the same hedging set
// offset in the effective notional, so the add-on is below the gross.
func TestSACCR_NettingWithinHedgingSet(t *testing.T) {
	long := SACCRTrade{AssetClass: Equity, HedgingSet: "ACME", AdjustedNotional: 100, Delta: 1, MaturityYears: 1}
	short := SACCRTrade{AssetClass: Equity, HedgingSet: "ACME", AdjustedNotional: 60, Delta: -1, MaturityYears: 1}
	hedged := SACCRNettingSet{Trades: []SACCRTrade{long, short}}.EAD()
	grossLong := SACCRNettingSet{Trades: []SACCRTrade{long}}.EAD()
	if hedged >= grossLong {
		t.Fatalf("a partial hedge must lower EAD: %.4f !< %.4f", hedged, grossLong)
	}
}

// TestOptionDelta: a deep ITM call ≈ +1, a deep OTM call ≈ 0, a short put ≈ +1.
func TestOptionDelta(t *testing.T) {
	if d := OptionDelta(true, true, 200, 100, 0.3, 1); d < 0.9 {
		t.Fatalf("deep-ITM call delta should be ≈1, got %.4f", d)
	}
	if d := OptionDelta(true, true, 50, 100, 0.3, 1); d > 0.2 {
		t.Fatalf("deep-OTM call delta should be ≈0, got %.4f", d)
	}
	if d := OptionDelta(false, false, 100, 100, 0.3, 1); d <= 0 {
		t.Fatalf("short put supervisory delta should be positive, got %.4f", d)
	}
}
