package factormodel

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// FACTOR-01c — the statistical factor model (PCA) and the model selector. Where
// the fundamental model imposes observed loadings, the statistical model lets the
// data speak: it eigendecomposes the instrument return covariance and keeps the
// top-K principal components as latent factors. It is the model-agnostic
// complement — it captures common structure a fundamental factor set misses (the
// reason the BLEND runs PCA on the fundamental model's residuals).

// ModelType selects which factor model Fit builds.
type ModelType int

const (
	// Fundamental: observed style/industry/country loadings, factor returns by
	// cross-sectional regression (FACTOR-01b).
	Fundamental ModelType = iota
	// Statistical: PCA on the return covariance (FACTOR-01c).
	Statistical
	// Blend: the fundamental factors PLUS statistical factors fit on the
	// fundamental residuals (orthogonal by construction) — the hybrid that keeps
	// interpretable factors while capturing leftover common structure.
	Blend
)

// Estimation defaults.
const (
	DefaultWindow      = 250
	DefaultStatFactors = 3
	// DefaultRidge regularizes the cross-sectional normal equations so collinear
	// industry/country dummies stay invertible; tiny relative to BᵀB so it does
	// not bias a well-conditioned fit.
	DefaultRidge = 1e-8
)

// Config parameterizes the factor-model estimation.
type Config struct {
	Type ModelType
	// Window is the return lookback for the regression / PCA. 0 ⇒ DefaultWindow.
	Window int
	// StyleFactors are the ordered style-characteristic names the fundamental
	// model builds z-scored loadings from.
	StyleFactors []string
	// StatFactors is the number of principal components (Statistical) or extra
	// blend factors. 0 ⇒ DefaultStatFactors.
	StatFactors int
	// Ridge regularizes the cross-sectional OLS. 0 ⇒ DefaultRidge.
	Ridge float64
}

func (c Config) window() int {
	if c.Window <= 0 {
		return DefaultWindow
	}
	return c.Window
}

func (c Config) ridge() float64 {
	if c.Ridge <= 0 {
		return DefaultRidge
	}
	return c.Ridge
}

func (c Config) statFactors() int {
	if c.StatFactors <= 0 {
		return DefaultStatFactors
	}
	return c.StatFactors
}

// Providers bundles the data seams the estimators read. Characteristics may be
// nil for a Statistical-only fit (PCA needs only returns).
type Providers struct {
	Characteristics CharacteristicProvider
	Returns         ReturnsProvider
}

// Fit builds the configured factor model over the instrument universe as of
// asOf. The universe is sorted internally for a deterministic factor model
// (same inputs ⇒ identical model, the EVT-21d replay property).
func Fit(ctx context.Context, cfg Config, instruments []string, asOf time.Time, p Providers) (*Model, error) {
	if p.Returns == nil {
		return nil, fmt.Errorf("factormodel: a ReturnsProvider is required")
	}
	switch cfg.Type {
	case Statistical:
		universe := append([]string(nil), instruments...)
		sort.Strings(universe)
		returns, err := alignedReturns(ctx, p.Returns, universe, asOf, cfg.window())
		if err != nil {
			return nil, err
		}
		return fitStatistical(universe, returns, cfg.statFactors(), "PC"), nil
	case Blend:
		if p.Characteristics == nil {
			return nil, fmt.Errorf("factormodel: Blend requires a CharacteristicProvider")
		}
		fund, residuals, err := fitFundamental(ctx, cfg, instruments, asOf, p.Characteristics, p.Returns)
		if err != nil {
			return nil, err
		}
		stat := fitStatistical(fund.Instruments, residuals, cfg.statFactors(), "SPC")
		return blend(fund, stat), nil
	default: // Fundamental
		if p.Characteristics == nil {
			return nil, fmt.Errorf("factormodel: Fundamental requires a CharacteristicProvider")
		}
		m, _, err := fitFundamental(ctx, cfg, instruments, asOf, p.Characteristics, p.Returns)
		return m, err
	}
}

// fitStatistical runs PCA on the return covariance and keeps the top-k principal
// components as factors. Loadings are the eigenvector components, the factor
// covariance is diagonal in the (uncorrelated) eigenvalues, and the specific
// variance is each instrument's total variance net of what the kept components
// explain. namePrefix labels the factors ("PC" / "SPC" for the blend residuals).
func fitStatistical(instruments []string, returns [][]float64, k int, namePrefix string) *Model {
	n := len(instruments)
	cov := sampleCov(returns)
	vals, vecs := eigenSym(cov)

	// Order eigenpairs by descending eigenvalue; keep the top k (≤ n).
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return vals[order[a]] > vals[order[b]] })
	if k > n {
		k = n
	}

	factors := make([]Factor, k)
	loadings := make([][]float64, n)
	for i := range loadings {
		loadings[i] = make([]float64, k)
	}
	factorCov := make([][]float64, k)
	for j := 0; j < k; j++ {
		factorCov[j] = make([]float64, k)
		lambda := vals[order[j]]
		if lambda < 0 {
			lambda = 0 // round-off on a near-singular covariance
		}
		factorCov[j][j] = lambda
		factors[j] = Factor{Name: fmt.Sprintf("%s%d", namePrefix, j+1), Type: FactorStatistical}
		vec := vecs[order[j]]
		for i := 0; i < n; i++ {
			loadings[i][j] = vec[i]
		}
	}

	// Specific variance = total variance − variance explained by the kept PCs.
	specific := make(map[string]float64, n)
	for i, id := range instruments {
		explained := 0.0
		for j := 0; j < k; j++ {
			explained += loadings[i][j] * loadings[i][j] * factorCov[j][j]
		}
		s := cov[i][i] - explained
		if s < 0 {
			s = 0
		}
		specific[id] = s
	}
	return newModel(factors, instruments, loadings, factorCov, specific)
}

// blend concatenates a fundamental model with a statistical model fit on its
// residuals into one model: stacked loadings, a block-diagonal factor covariance
// (the two factor blocks are orthogonal in-sample — the PCs are of the residuals,
// which the OLS makes orthogonal to the fundamental factors), and the residual-
// of-residual specific variance from the statistical pass.
func blend(fund, stat *Model) *Model {
	kf, ks := len(fund.Factors), len(stat.Factors)
	k := kf + ks
	factors := make([]Factor, 0, k)
	factors = append(factors, fund.Factors...)
	factors = append(factors, stat.Factors...)

	n := len(fund.Instruments)
	loadings := make([][]float64, n)
	for i := 0; i < n; i++ {
		row := make([]float64, 0, k)
		row = append(row, fund.Loadings[i]...)
		row = append(row, stat.Loadings[i]...)
		loadings[i] = row
	}

	factorCov := make([][]float64, k)
	for a := 0; a < k; a++ {
		factorCov[a] = make([]float64, k)
	}
	for a := 0; a < kf; a++ {
		copy(factorCov[a][:kf], fund.FactorCov[a])
	}
	for a := 0; a < ks; a++ {
		copy(factorCov[kf+a][kf:], stat.FactorCov[a])
	}
	// The statistical pass on the residuals leaves the truly idiosyncratic part.
	return newModel(factors, fund.Instruments, loadings, factorCov, stat.SpecificVar)
}

// alignedReturns reads each instrument's return series and tail-aligns them to a
// common length (the shortest series with ≥2 points), so the covariance/
// regression sees one rectangular N×L panel. An instrument with no (or too
// little) history gets a zero row — it contributes no factor/PC structure and a
// zero specific variance, surfaced as a coverage concern a layer up.
func alignedReturns(ctx context.Context, rp ReturnsProvider, instruments []string, asOf time.Time, window int) ([][]float64, error) {
	series := make([][]float64, len(instruments))
	minLen := -1
	for i, id := range instruments {
		r, err := rp.Returns(ctx, id, asOf, window)
		if err != nil {
			return nil, err
		}
		series[i] = r
		if len(r) >= 2 && (minLen < 0 || len(r) < minLen) {
			minLen = len(r)
		}
	}
	if minLen < 2 {
		return nil, fmt.Errorf("factormodel: insufficient return history to estimate a covariance")
	}
	out := make([][]float64, len(instruments))
	for i, r := range series {
		row := make([]float64, minLen)
		if len(r) >= minLen {
			copy(row, r[len(r)-minLen:]) // tail-align
		}
		out[i] = row
	}
	return out, nil
}
