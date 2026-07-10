package returns

import (
	"context"
	"math"
	"time"
)

// DefaultVolMinObs is the minimum number of returns required to estimate a
// volatility. Below it, Volatility reports ok=false rather than a stddev off a
// handful of points (which the sample stddev would estimate far too noisily to
// trust).
const DefaultVolMinObs = 20

// ReturnsVolModel estimates an instrument's one-sigma periodic return
// volatility as the sample standard deviation of its historical return series,
// read from a ReturnsProvider. It satisfies the compute.VolModel interface
// structurally, so the risk engine's uncertainty path (PopulateUncertainty) and
// the feature dataset both estimate volatility off one consistent data source —
// the same returns that drive VaR.
type ReturnsVolModel struct {
	rp     ReturnsProvider
	window int
	minObs int
}

// NewReturnsVolModel builds a model over rp. A non-positive window takes the
// historical-returns default (DefaultReturnWindow); minObs takes
// DefaultVolMinObs.
func NewReturnsVolModel(rp ReturnsProvider, window int) *ReturnsVolModel {
	if window <= 0 {
		window = DefaultReturnWindow
	}
	return &ReturnsVolModel{rp: rp, window: window, minObs: DefaultVolMinObs}
}

// Volatility returns the one-sigma return volatility for instrumentID as of
// asOf via the sample standard deviation of its returns. ok=false when there is
// insufficient history (the caller leaves that position's uncertainty nil
// rather than fabricating a zero band). An error is a genuine data-plane
// failure — the caller skips the position so a transient store fault degrades to
// "no uncertainty" rather than failing the whole recompute.
func (m *ReturnsVolModel) Volatility(ctx context.Context, instrumentID string, asOf time.Time) (float64, bool, error) {
	rets, err := m.rp.Returns(ctx, instrumentID, asOf, m.window)
	if err != nil {
		return 0, false, err
	}
	if len(rets) < m.minObs {
		return 0, false, nil
	}
	return sampleStdDev(rets), true, nil
}

// sampleStdDev returns the unbiased (n-1) sample standard deviation of xs, or 0
// for fewer than two points.
func sampleStdDev(xs []float64) float64 {
	n := len(xs)
	if n < 2 {
		return 0
	}
	var mean float64
	for _, x := range xs {
		mean += x
	}
	mean /= float64(n)
	var ss float64
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	return math.Sqrt(ss / float64(n-1))
}
