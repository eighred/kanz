package performance

import "testing"

func TestExPostRisk_BetaAndTrackingError(t *testing.T) {
	bench := []float64{0.01, -0.02, 0.03, -0.01, 0.02}
	port := make([]float64, len(bench))
	for i, b := range bench {
		port[i] = 2 * b // perfectly correlated, 2× ⇒ beta 2
	}
	stats := ExPostRisk(port, bench, 0, 252)
	near(t, "beta", stats.Beta, 2.0, 1e-9)
	if stats.TrackingError <= 0 {
		t.Fatalf("tracking error should be positive when port ≠ bench, got %.6f", stats.TrackingError)
	}
	if stats.Periods != 5 {
		t.Fatalf("periods: got %d want 5", stats.Periods)
	}
}

func TestExPostRisk_ZeroActiveWhenIdentical(t *testing.T) {
	r := []float64{0.01, -0.02, 0.03}
	stats := ExPostRisk(r, r, 0, 252)
	near(t, "tracking error zero", stats.TrackingError, 0, 0)
	near(t, "beta one", stats.Beta, 1.0, 1e-9)
}

func TestExPostRisk_SharpeSign(t *testing.T) {
	// Positive mean excess return ⇒ positive Sharpe.
	port := []float64{0.02, 0.01, 0.03, 0.015}
	bench := []float64{0.0, 0.0, 0.0, 0.0}
	stats := ExPostRisk(port, bench, 0, 252)
	if stats.Sharpe <= 0 {
		t.Fatalf("Sharpe should be positive for a positive-mean series, got %.4f", stats.Sharpe)
	}
}

func TestExPostRisk_InsufficientData(t *testing.T) {
	stats := ExPostRisk([]float64{0.01}, []float64{0.01}, 0, 252)
	if stats != (RiskStats{Periods: 1}) {
		t.Fatalf("single observation must yield zero-value stats, got %+v", stats)
	}
}
