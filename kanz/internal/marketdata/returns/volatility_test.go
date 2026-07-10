package returns

import (
	"context"
	"math"
	"testing"
	"time"
)

var volAsOf = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// volStubProvider returns a fixed series for every instrument.
type volStubProvider struct {
	series []float64
	err    error
}

func (p *volStubProvider) Returns(_ context.Context, _ string, _ time.Time, _ int) ([]float64, error) {
	return p.series, p.err
}

// expectedStdDev mirrors the model's unbiased (n-1) sample stddev so the test
// asserts against an independently-computed expectation, not a magic number.
func expectedStdDev(xs []float64) float64 {
	n := float64(len(xs))
	var mean float64
	for _, x := range xs {
		mean += x
	}
	mean /= n
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return math.Sqrt(ss / (n - 1))
}

// ramp builds a deterministic n-length return series with non-trivial spread.
func ramp(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = float64(i%7-3) * 0.01 // -0.03..0.03, varied
	}
	return out
}

func TestReturnsVolModel_VolatilityIsSampleStdDev(t *testing.T) {
	series := ramp(30)
	vm := NewReturnsVolModel(&volStubProvider{series: series}, 0)

	sigma, ok, err := vm.Volatility(context.Background(), "AAPL", volAsOf)
	if err != nil || !ok {
		t.Fatalf("Volatility ok=%v err=%v", ok, err)
	}
	if want := expectedStdDev(series); math.Abs(sigma-want) > 1e-12 {
		t.Errorf("sigma = %v want %v", sigma, want)
	}
}

func TestReturnsVolModel_InsufficientHistory(t *testing.T) {
	// Fewer than DefaultVolMinObs returns ⇒ ok=false (no fabricated band).
	vm := NewReturnsVolModel(&volStubProvider{series: ramp(5)}, 0)
	if _, ok, err := vm.Volatility(context.Background(), "AAPL", volAsOf); ok || err != nil {
		t.Fatalf("short history: ok=%v err=%v want false,nil", ok, err)
	}
}
