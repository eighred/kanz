package collateral

import (
	"math"
	"testing"
)

// TestSIMM_SingleBucket reproduces the hand-derived delta-margin aggregation for
// one bucket with two risk factors:
//
//	WS = [0.2·100, 0.2·(−50)] = [20, −10]
//	K  = √(20² + 10² + 2·0.5·20·(−10)) = √300 = 17.32051
//	one bucket ⇒ IM = K
func TestSIMM_SingleBucket(t *testing.T) {
	sens := []Sensitivity{
		{RiskClass: "Equity", Bucket: "A", RiskFactor: "X", Amount: 100},
		{RiskClass: "Equity", Bucket: "A", RiskFactor: "Y", Amount: -50},
	}
	p := SIMMParams{RiskWeight: map[string]float64{"A": 0.2}, IntraBucketCorr: 0.5}
	if im := SIMM(sens, p); math.Abs(im-math.Sqrt(300)) > 1e-6 {
		t.Fatalf("IM: got %.6f want %.6f", im, math.Sqrt(300))
	}
}

// TestSIMM_TwoBuckets adds a second bucket and the across-bucket correlation:
//
//	K_A = √300, S_A = 10 ; K_B = 0.2·80 = 16, S_B = 16
//	IM  = √( K_A² + K_B² + γ·2·S_A·S_B ) = √(300 + 256 + 0.3·320) = √652
func TestSIMM_TwoBuckets(t *testing.T) {
	sens := []Sensitivity{
		{RiskClass: "Equity", Bucket: "A", RiskFactor: "X", Amount: 100},
		{RiskClass: "Equity", Bucket: "A", RiskFactor: "Y", Amount: -50},
		{RiskClass: "Equity", Bucket: "B", RiskFactor: "Z", Amount: 80},
	}
	p := SIMMParams{
		RiskWeight:      map[string]float64{"A": 0.2, "B": 0.2},
		IntraBucketCorr: 0.5,
		InterBucketCorr: 0.3,
	}
	if im := SIMM(sens, p); math.Abs(im-math.Sqrt(652)) > 1e-6 {
		t.Fatalf("IM: got %.6f want %.6f", im, math.Sqrt(652))
	}
}

// TestSIMM_NettingReducesMargin: an offsetting sensitivity to the same factor
// nets before weighting, lowering the margin.
func TestSIMM_NettingReducesMargin(t *testing.T) {
	p := SIMMParams{RiskWeight: map[string]float64{"A": 0.2}, IntraBucketCorr: 0.5}
	gross := SIMM([]Sensitivity{{Bucket: "A", RiskFactor: "X", Amount: 100}}, p)
	netted := SIMM([]Sensitivity{
		{Bucket: "A", RiskFactor: "X", Amount: 100},
		{Bucket: "A", RiskFactor: "X", Amount: -60},
	}, p)
	if netted >= gross {
		t.Fatalf("an offsetting sensitivity must reduce SIMM: %.4f !< %.4f", netted, gross)
	}
}
