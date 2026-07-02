package alternatives

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// Calibrated alternatives inputs (PARITY-03e). ALT-01 shipped the math behind
// hand-supplied inputs: ProxyMapping betas were asserted, PME took an abstract
// benchmarkLevel func, and there was no peer context for a fund's IRR/TVPI.
// This file makes those inputs empirical:
//
//   - CalibrateProxy estimates a strategy's public-factor betas by OLS on
//     observed return series — the "levered small-cap" buyout proxy becomes a
//     regression result with an R², not folklore.
//   - BenchmarkSeries turns the live PERF-01c benchmark index levels into the
//     level func PME consumes, point-in-time (a date before the first
//     observation yields 0 so PME fails loudly rather than silently rescaling).
//   - VintageBenchmark ranks a fund against its vintage-year peer group by
//     IRR/TVPI quartile — the standard private-markets relative benchmark.

// ErrProxyCalibration is returned when the regression cannot run (no factors,
// misaligned series, too few observations, or collinear factor returns).
var ErrProxyCalibration = errors.New("alternatives: cannot calibrate proxy")

// CalibrateProxy regresses a strategy's return series on the named public-
// factor return series (all aligned, same period grid) and returns the fitted
// ProxyMapping plus the regression R². The regression includes an intercept
// (the strategy's alpha) which is NOT part of the mapping — proxy exposure is
// systematic loadings only. Requires more observations than parameters.
func CalibrateProxy(name string, strategy []float64, factors map[string][]float64) (ProxyMapping, float64, error) {
	if len(factors) == 0 {
		return ProxyMapping{}, 0, fmt.Errorf("%w: no factor series", ErrProxyCalibration)
	}
	n := len(strategy)
	names := make([]string, 0, len(factors))
	for f := range factors {
		names = append(names, f)
	}
	sort.Strings(names) // deterministic design-matrix order
	for _, f := range names {
		if len(factors[f]) != n {
			return ProxyMapping{}, 0, fmt.Errorf("%w: factor %q has %d observations, strategy has %d", ErrProxyCalibration, f, len(factors[f]), n)
		}
	}
	k := len(names) + 1 // + intercept
	if n <= k {
		return ProxyMapping{}, 0, fmt.Errorf("%w: %d observations for %d parameters", ErrProxyCalibration, n, k)
	}

	// Design matrix row: [1, f_1 … f_K]; solve (XᵀX)β = Xᵀy by Gaussian
	// elimination with partial pivoting.
	xtx := make([][]float64, k)
	xty := make([]float64, k)
	for i := range xtx {
		xtx[i] = make([]float64, k)
	}
	row := make([]float64, k)
	for t := 0; t < n; t++ {
		row[0] = 1
		for j, f := range names {
			row[j+1] = factors[f][t]
		}
		for i := 0; i < k; i++ {
			for j := 0; j < k; j++ {
				xtx[i][j] += row[i] * row[j]
			}
			xty[i] += row[i] * strategy[t]
		}
	}
	beta, ok := solveGaussian(xtx, xty)
	if !ok {
		return ProxyMapping{}, 0, fmt.Errorf("%w: collinear factor returns (singular normal equations)", ErrProxyCalibration)
	}

	// R² against the mean model.
	var mean float64
	for _, y := range strategy {
		mean += y
	}
	mean /= float64(n)
	var ssr, sst float64
	for t := 0; t < n; t++ {
		fitted := beta[0]
		for j := range names {
			fitted += beta[j+1] * factors[names[j]][t]
		}
		ssr += (strategy[t] - fitted) * (strategy[t] - fitted)
		sst += (strategy[t] - mean) * (strategy[t] - mean)
	}
	r2 := 0.0
	if sst > 0 {
		r2 = 1 - ssr/sst
	}

	betas := make(map[string]float64, len(names))
	for j, f := range names {
		betas[f] = beta[j+1]
	}
	return ProxyMapping{Name: name, Betas: betas}, r2, nil
}

// solveGaussian solves Ax=b in place with partial pivoting; ok=false when the
// system is singular to working precision.
func solveGaussian(a [][]float64, b []float64) ([]float64, bool) {
	n := len(a)
	for col := 0; col < n; col++ {
		pivot := col
		for r := col + 1; r < n; r++ {
			if math.Abs(a[r][col]) > math.Abs(a[pivot][col]) {
				pivot = r
			}
		}
		if math.Abs(a[pivot][col]) < 1e-12 {
			return nil, false
		}
		a[col], a[pivot] = a[pivot], a[col]
		b[col], b[pivot] = b[pivot], b[col]
		for r := col + 1; r < n; r++ {
			f := a[r][col] / a[col][col]
			for c := col; c < n; c++ {
				a[r][c] -= f * a[col][c]
			}
			b[r] -= f * b[col]
		}
	}
	x := make([]float64, n)
	for r := n - 1; r >= 0; r-- {
		x[r] = b[r]
		for c := r + 1; c < n; c++ {
			x[r] -= a[r][c] * x[c]
		}
		x[r] /= a[r][r]
	}
	return x, true
}

// BenchmarkSeries is an observed public-benchmark index-level series — the
// live PERF-01c levels behind PME's benchmarkLevel input. Lookup is
// point-in-time: the level at or before the date; a date before the first
// observation yields 0, which PME rejects loudly (never a silent rescale).
type BenchmarkSeries struct {
	dates  []time.Time // ascending
	levels []float64
}

// ErrBenchmarkSeries is returned for an unusable level series.
var ErrBenchmarkSeries = errors.New("alternatives: invalid benchmark series")

// NewBenchmarkSeries builds a series from aligned dates and positive levels.
// Observations need not be pre-sorted; duplicate dates are rejected.
func NewBenchmarkSeries(dates []time.Time, levels []float64) (*BenchmarkSeries, error) {
	if len(dates) == 0 || len(dates) != len(levels) {
		return nil, fmt.Errorf("%w: need equal, non-empty dates and levels", ErrBenchmarkSeries)
	}
	type obs struct {
		d time.Time
		l float64
	}
	os := make([]obs, len(dates))
	for i := range dates {
		if levels[i] <= 0 {
			return nil, fmt.Errorf("%w: non-positive level at %s", ErrBenchmarkSeries, dates[i].Format("2006-01-02"))
		}
		os[i] = obs{dates[i], levels[i]}
	}
	sort.Slice(os, func(i, j int) bool { return os[i].d.Before(os[j].d) })
	s := &BenchmarkSeries{dates: make([]time.Time, len(os)), levels: make([]float64, len(os))}
	for i, o := range os {
		if i > 0 && !os[i-1].d.Before(o.d) {
			return nil, fmt.Errorf("%w: duplicate date %s", ErrBenchmarkSeries, o.d.Format("2006-01-02"))
		}
		s.dates[i], s.levels[i] = o.d, o.l
	}
	return s, nil
}

// Level returns the index level at or before t (0 before the first
// observation — an error signal for PME, not a level).
func (s *BenchmarkSeries) Level(t time.Time) float64 {
	i := sort.Search(len(s.dates), func(i int) bool { return s.dates[i].After(t) })
	if i == 0 {
		return 0
	}
	return s.levels[i-1]
}

// LevelFunc adapts the series to PME's benchmarkLevel parameter.
func (s *BenchmarkSeries) LevelFunc() func(time.Time) float64 { return s.Level }

// PeerFund is one comparable fund in a vintage peer group.
type PeerFund struct {
	Vintage int // vintage year
	IRR     float64
	TVPI    float64
}

// Quartiles are linear-interpolated quartile breakpoints of a peer metric
// (Q1 = 25th percentile, Q3 = 75th).
type Quartiles struct {
	Q1, Median, Q3 float64
}

// PeerStats is the vintage peer benchmark: quartile breakpoints per metric
// over the N peers of one vintage year.
type PeerStats struct {
	Vintage int
	N       int
	IRR     Quartiles
	TVPI    Quartiles
}

// ErrPeerGroup is returned when a vintage has too few peers to benchmark.
var ErrPeerGroup = errors.New("alternatives: vintage peer group too small")

// minPeers is the smallest peer group with meaningful quartiles.
const minPeers = 4

// VintageBenchmark builds the peer benchmark for a vintage year from the peer
// universe (funds of other vintages are ignored — vintage-mixing is the
// classic private-markets benchmarking error).
func VintageBenchmark(peers []PeerFund, vintage int) (PeerStats, error) {
	var irrs, tvpis []float64
	for _, p := range peers {
		if p.Vintage == vintage {
			irrs = append(irrs, p.IRR)
			tvpis = append(tvpis, p.TVPI)
		}
	}
	if len(irrs) < minPeers {
		return PeerStats{}, fmt.Errorf("%w: %d funds of vintage %d (need ≥%d)", ErrPeerGroup, len(irrs), vintage, minPeers)
	}
	sort.Float64s(irrs)
	sort.Float64s(tvpis)
	return PeerStats{
		Vintage: vintage,
		N:       len(irrs),
		IRR:     Quartiles{percentile(irrs, 0.25), percentile(irrs, 0.5), percentile(irrs, 0.75)},
		TVPI:    Quartiles{percentile(tvpis, 0.25), percentile(tvpis, 0.5), percentile(tvpis, 0.75)},
	}, nil
}

// Quartile ranks a metric value against breakpoints: 1 (top quartile, ≥ Q3)
// through 4 (bottom, < Q1) — the LP-report convention.
func (q Quartiles) Quartile(x float64) int {
	switch {
	case x >= q.Q3:
		return 1
	case x >= q.Median:
		return 2
	case x >= q.Q1:
		return 3
	default:
		return 4
	}
}

// percentile is the linear-interpolated percentile of sorted xs.
func percentile(xs []float64, p float64) float64 {
	pos := p * float64(len(xs)-1)
	lo := int(math.Floor(pos))
	if lo >= len(xs)-1 {
		return xs[len(xs)-1]
	}
	frac := pos - float64(lo)
	return xs[lo] + frac*(xs[lo+1]-xs[lo])
}
