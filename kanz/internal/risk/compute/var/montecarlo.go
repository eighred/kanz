package varmodel

import (
	"context"
	"math"
	"math/rand"
	"sort"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
)

// MODEL-01e: Monte-Carlo VaR over a correlated factor-shock model. Where
// Historical (MODEL-01d) resamples the actual past P&Ls, MonteCarlo fits a
// multivariate-normal model to the historical returns — mean vector + full
// instrument covariance — and draws many correlated synthetic scenarios from
// it. The covariance IS the factor structure: cross-instrument correlation is
// estimated from the same return series the provider supplies, so a
// diversified book gets a lower VaR than a concentrated one (a property
// Historical only captures to the extent the realized window happened to show
// it). A reduced-rank factor model (sector/style factors) is MODEL-01f; this
// uses the full empirical covariance.

// Monte-Carlo defaults.
const (
	DefaultDraws = 10000
	DefaultSeed  = 1
	// covJitter is added to the covariance diagonal when Cholesky hits a non-
	// positive pivot (a singular/degenerate covariance — e.g. perfectly
	// correlated or constant series). It keeps the factorization finite without
	// materially changing a well-conditioned matrix.
	covJitter = 1e-12
)

// MonteCarlo builds a Monte-Carlo VaR measure. It is registered exactly like
// Historical (via BindReturns / RegisterMonteCarlo) — an interchangeable
// MeasureVaR99 implementation. Deterministic given inputs + Config.Seed, so a
// replayed recompute reproduces the same number (EVT-21d determinism).
func MonteCarlo(cfg Config) compute.ReturnsMeasure {
	conf := cfg.confidence()
	window := cfg.Window
	draws := cfg.Draws
	if draws <= 0 {
		draws = DefaultDraws
	}
	seed := cfg.Seed
	if seed == 0 {
		seed = DefaultSeed
	}
	return func(ctx context.Context, p *domain.Portfolio, rp compute.ReturnsProvider) v1.Measure {
		base := p.BaseCurrency()
		var values []float64 // position value per instrument
		var series [][]float64
		minLen := -1
		for _, pos := range p.Positions() {
			if !pos.InBaseCurrency(base) {
				continue
			}
			r, err := rp.Returns(ctx, string(pos.InstrumentID), p.AsOf(), window)
			if err != nil || len(r) == 0 {
				continue
			}
			values = append(values, decimalToFloat(pos.MarketValue.Amount))
			series = append(series, r)
			if minLen < 0 || len(r) < minLen {
				minLen = len(r)
			}
		}
		// Need ≥2 scenarios for a sample covariance.
		if len(series) == 0 || minLen < 2 {
			return zeroMeasure()
		}

		// Tail-align every series to the common window (matching Historical), so
		// the covariance is estimated over aligned scenarios.
		n := len(series)
		r := make([][]float64, n) // r[i] = aligned returns of instrument i
		for i, s := range series {
			r[i] = s[len(s)-minLen:]
		}

		mean := rowMeans(r)
		cov := covariance(r, mean)
		chol := cholesky(cov)

		rng := rand.New(rand.NewSource(seed))
		pnl := make([]float64, draws)
		z := make([]float64, n)
		for d := 0; d < draws; d++ {
			for i := range z {
				z[i] = rng.NormFloat64()
			}
			var loss float64
			for i := 0; i < n; i++ {
				// sim_i = mean_i + (chol · z)_i  (chol lower-triangular)
				sim := mean[i]
				for j := 0; j <= i; j++ {
					sim += chol[i][j] * z[j]
				}
				loss += values[i] * sim
			}
			pnl[d] = loss
		}
		sort.Float64s(pnl)

		v := -quantile(pnl, 1-conf)
		if v < 0 {
			v = 0
		}
		return v1.Measure{
			Name:  compute.MeasureVaR99,
			Value: floatToDecimal(v, varExponent),
		}
	}
}

// RegisterMonteCarlo overrides MeasureVaR99 in r with Monte-Carlo VaR closed
// over provider — the Monte-Carlo counterpart of Register.
func RegisterMonteCarlo(ctx context.Context, r *compute.Registry, provider compute.ReturnsProvider, cfg Config) {
	r.Register(compute.MeasureVaR99, compute.BindReturns(ctx, provider, MonteCarlo(cfg)))
}

// rowMeans returns the per-instrument mean return.
func rowMeans(r [][]float64) []float64 {
	mean := make([]float64, len(r))
	for i, row := range r {
		var s float64
		for _, v := range row {
			s += v
		}
		mean[i] = s / float64(len(row))
	}
	return mean
}

// covariance returns the n×n sample covariance matrix (Bessel-corrected,
// 1/(L-1)) of the aligned return rows.
func covariance(r [][]float64, mean []float64) [][]float64 {
	n := len(r)
	l := len(r[0])
	cov := make([][]float64, n)
	for i := range cov {
		cov[i] = make([]float64, n)
	}
	denom := float64(l - 1)
	for i := 0; i < n; i++ {
		for j := i; j < n; j++ {
			var s float64
			for t := 0; t < l; t++ {
				s += (r[i][t] - mean[i]) * (r[j][t] - mean[j])
			}
			c := s / denom
			cov[i][j] = c
			cov[j][i] = c
		}
	}
	return cov
}

// cholesky returns the lower-triangular L with L·Lᵀ = cov, adding covJitter to
// a non-positive pivot so a singular/degenerate covariance still factorizes
// (the correlated draws then degenerate gracefully rather than producing NaN).
func cholesky(cov [][]float64) [][]float64 {
	n := len(cov)
	l := make([][]float64, n)
	for i := range l {
		l[i] = make([]float64, n)
	}
	for i := 0; i < n; i++ {
		for j := 0; j <= i; j++ {
			sum := cov[i][j]
			for k := 0; k < j; k++ {
				sum -= l[i][k] * l[j][k]
			}
			if i == j {
				if sum <= 0 {
					sum = covJitter
				}
				l[i][i] = math.Sqrt(sum)
			} else {
				l[i][j] = sum / l[j][j]
			}
		}
	}
	return l
}
