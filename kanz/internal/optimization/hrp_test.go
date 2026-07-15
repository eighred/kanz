package optimization

import (
	"math"
	"testing"
)

// hrpSum is a local helper (sumWeights in optimizer_test.go operates on maps).
func hrpSum(w []float64) float64 {
	var s float64
	for _, v := range w {
		s += v
	}
	return s
}

// Two uncorrelated assets: HRP reduces to the inverse-variance split. σ²=0.01,0.04
// ⇒ w ∝ [1/0.01, 1/0.04] = [100,25] ⇒ [0.8, 0.2].
func TestHRP_TwoAssetInverseVariance(t *testing.T) {
	w := hrp(diag(0.01, 0.04))
	approx(t, "w0", w[0], 0.8, 1e-9)
	approx(t, "w1", w[1], 0.2, 1e-9)
	approx(t, "sum", hrpSum(w), 1, 1e-12)
}

// Four assets, two correlated pairs: (A,B) var 0.04 ρ=0.9, (C,D) var 0.16 ρ=0.9,
// cross-correlation 0. Clustering groups {A,B} and {C,D}; the ordered list
// [A,B,C,D] bisects on the cluster boundary. Cluster variances V_AB=0.038,
// V_CD=0.152 ⇒ top split α=1−0.038/0.19=0.8 to {A,B}; within each pair the equal
// vars split 0.5/0.5. HRP weights = [0.4, 0.4, 0.1, 0.1] exactly.
func TestHRP_TwoClusterReference(t *testing.T) {
	cov := [][]float64{
		{0.04, 0.036, 0, 0},
		{0.036, 0.04, 0, 0},
		{0, 0, 0.16, 0.144},
		{0, 0, 0.144, 0.16},
	}
	w := hrp(cov)
	approx(t, "w_A", w[0], 0.4, 1e-9)
	approx(t, "w_B", w[1], 0.4, 1e-9)
	approx(t, "w_C", w[2], 0.1, 1e-9)
	approx(t, "w_D", w[3], 0.1, 1e-9)
	approx(t, "sum", hrpSum(w), 1, 1e-12)
}

// Diversification vs min-variance: min-variance concentrates in the
// low-variance {A,B} cluster (C,D have 4× the variance) and additionally
// exploits the cross-cluster covariance (0.02) to hedge, while HRP's
// clusterVar is computed from intra-cluster submatrices only and never sees
// the cross term — a deliberate blind spot (Σ is never inverted) that is the
// whole point of HRP's numerical robustness. So w_HRP(C)+w_HRP(D) must exceed
// w_minvar(C)+w_minvar(D). NOTE: a block-diagonal (zero cross-covariance) cov
// cannot exercise this property — with no cross-cluster information for
// inversion to exploit, HRP's local bisection and full min-variance are
// provably identical at the cluster level (verified analytically and
// empirically: v1/(v1+v2) for both, independent of intra-cluster correlation),
// so the cross term above is required, not decorative.
func TestHRP_LessConcentratedThanMinVariance(t *testing.T) {
	cov := [][]float64{
		{0.04, 0.036, 0.02, 0.02},
		{0.036, 0.04, 0.02, 0.02},
		{0.02, 0.02, 0.16, 0.144},
		{0.02, 0.02, 0.144, 0.16},
	}
	in := MarketInputs{Instruments: []string{"A", "B", "C", "D"}, Covariance: cov}
	mv, err := Optimize(in, Objective{Type: MinVariance}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := hrp(cov)
	hrpHighVol := w[2] + w[3]
	mvHighVol := mv.Weights["C"] + mv.Weights["D"]
	if !(hrpHighVol > mvHighVol) {
		t.Fatalf("HRP should keep more in the high-vol cluster than min-variance: HRP=%.4f minvar=%.4f", hrpHighVol, mvHighVol)
	}
}

func TestHRP_Invariants(t *testing.T) {
	covs := [][][]float64{
		diag(0.02, 0.05, 0.09),
		{{0.04, 0.01, 0.005}, {0.01, 0.03, 0.002}, {0.005, 0.002, 0.06}},
	}
	for idx, cov := range covs {
		w := hrp(cov)
		if math.Abs(hrpSum(w)-1) > 1e-9 {
			t.Errorf("cov %d: weights must sum to 1, got %.9f", idx, hrpSum(w))
		}
		for i, v := range w {
			if v < 0 || math.IsNaN(v) {
				t.Errorf("cov %d: weight %d not long-only/finite: %v", idx, i, v)
			}
		}
	}
}

func TestHRP_EdgeCases(t *testing.T) {
	// n==1 ⇒ all weight in the single asset.
	if w := hrp(diag(0.04)); len(w) != 1 || math.Abs(w[0]-1) > 1e-12 {
		t.Fatalf("n==1 ⇒ [1.0], got %v", w)
	}
	// One zero-variance asset: finite, long-only, sums to 1 (no NaN/panic).
	w := hrp(diag(0.0, 0.04, 0.04))
	if math.Abs(hrpSum(w)-1) > 1e-9 {
		t.Fatalf("zero-variance asset: weights must still sum to 1, got %v", w)
	}
	for i, v := range w {
		if v < 0 || math.IsNaN(v) {
			t.Fatalf("zero-variance asset: weight %d not finite/long-only: %v", i, v)
		}
	}
	// All-zero-variance on a balanced 4-asset universe ⇒ every split 50/50 ⇒ 0.25 each.
	w4 := hrp(diag(0, 0, 0, 0))
	for i, v := range w4 {
		approx(t, "degenerate equal weight "+string(rune('A'+i)), v, 0.25, 1e-9)
	}
}
